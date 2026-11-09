package calls

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/voxmail/voxmail/internal/maintenance"
)

const (
	// maxSpokenBodyRunes preserves the long-standing cap on how much of an
	// email body is spoken on a call.
	maxSpokenBodyRunes = 2800
	// speechChunkRunes bounds one streamed synthesis so Piper never works on
	// a whole body at once; chunks of this size synthesize in a few seconds.
	speechChunkRunes = 350
	// singlePromptBodyRunes keeps short bodies in one prompt, where a single
	// synthesis round trip is cheaper than streaming.
	singlePromptBodyRunes = 600
	// bodyChunkSlots is the double buffer for streaming: one chunk plays
	// while the next one synthesizes.
	bodyChunkSlots = 2
)

func runeLen(value string) int { return len([]rune(value)) }

// splitSentences cuts text after sentence terminators (allowing closing
// quotes and brackets) and at line breaks, keeping the punctuation.
func splitSentences(text string) []string {
	var sentences []string
	var current strings.Builder
	runes := []rune(text)
	flush := func() {
		if trimmed := strings.TrimSpace(current.String()); trimmed != "" {
			sentences = append(sentences, trimmed)
		}
		current.Reset()
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		current.WriteRune(r)
		if r == '\n' {
			flush()
			continue
		}
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// Allow closing quotes and brackets directly after the terminator.
		j := i + 1
		for j < len(runes) && strings.ContainsRune("\"'\u00bb\u300d)]", runes[j]) {
			current.WriteRune(runes[j])
			j++
		}
		if j >= len(runes) || runes[j] == ' ' || runes[j] == '\t' || runes[j] == '\n' {
			flush()
			i = j - 1
		}
	}
	flush()
	return sentences
}

// splitSpeechChunks groups sentences into chunks of at most maxRunes runes so
// each chunk is a natural, short unit of speech. A single sentence longer than
// the limit is split at word boundaries.
func splitSpeechChunks(text string, maxRunes int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = speechChunkRunes
	}
	var chunks []string
	current := ""
	for _, sentence := range splitSentences(text) {
		if current != "" && runeLen(current)+1+runeLen(sentence) <= maxRunes {
			current += " " + sentence
			continue
		}
		if current != "" {
			chunks = append(chunks, current)
			current = ""
		}
		for runeLen(sentence) > maxRunes {
			limit := []rune(sentence)
			cut := maxRunes
			for cut > 0 && limit[cut-1] != ' ' {
				cut--
			}
			if cut == 0 {
				cut = maxRunes
			}
			chunks = append(chunks, strings.TrimSpace(string(limit[:cut])))
			sentence = strings.TrimSpace(string(limit[cut:]))
		}
		current = sentence
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

// truncateForSpeech cuts text at or before maxRunes, preferring the last
// sentence boundary and falling back to the last word boundary, so spoken
// truncation never ends mid-word.
func truncateForSpeech(text string, maxRunes int) (string, bool) {
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text, false
	}
	limit := runes[:maxRunes]
	best := -1
	for i, r := range limit {
		if r == '.' || r == '!' || r == '?' {
			best = i
		}
	}
	if best >= 0 && best > maxRunes/2 {
		return string(limit[:best+1]), true
	}
	cut := maxRunes
	for cut > 0 && limit[cut-1] != ' ' {
		cut--
	}
	if cut == 0 {
		cut = maxRunes
	}
	return strings.TrimRight(string(limit[:cut]), " \t\n"), true
}

// playSpeechChunks streams an already-split list of chunks: one chunk plays
// while the next synthesizes, so the caller hears continuous speech instead
// of one long silent synthesis. Any barge-in or hangup cancels the current
// chunk and stops the stream; the event worker then handles the queued key.
func (s *Service) playSpeechChunks(sess *session, chunks []string) {
	if s == nil || s.Media == nil || sess == nil || len(chunks) == 0 {
		return
	}
	if len(chunks) == 1 {
		s.prompt(sess, chunks[0])
		return
	}
	runtime := s.promptRuntime(sess)
	if runtime == nil {
		for _, chunk := range chunks {
			s.prompt(sess, chunk)
		}
		return
	}
	dir := s.Media.Dir
	if dir == "" {
		dir = filepath.Dir(sess.TxPath)
	}
	s.streamSpeechChunks(sess, dir, chunks,
		func(ctx context.Context, text, output string) error {
			return runtime.Synthesize(ctx, text, output)
		},
		func(ctx context.Context, path string) error {
			return s.Media.playWAVTail(ctx, sess.TxPath, path, pcmChunkDrainTail)
		})
}

// streamSpeechChunks owns the double-buffered streaming loop, split from
// playSpeechChunks so tests can drive it with fake synthesis and playback.
// The staleness check re-reads playSequence after every chunk this loop
// plays: playPrompt advances the sequence for each chunk, so comparing
// against a pre-loop snapshot made the stream stop after its first chunk.
func (s *Service) streamSpeechChunks(sess *session, dir string, chunks []string, synthesize func(context.Context, string, string) error, play func(context.Context, string) error) {
	s.streamSpeechChunksTracked(sess, dir, chunks, nil, synthesize, play)
}

// playBodyFrom streams the tracked chunks of the message page being read,
// starting at chunk index start. Each completed chunk advances the session's
// body index, so the pause, skip and speech-rate keys can resume exactly
// where the caller left off. The caller speaks the follow-up menu.
func (s *Service) playBodyFrom(sess *session, start int) {
	if s == nil || sess == nil {
		return
	}
	s.mu.Lock()
	chunks := append([]string(nil), sess.BodyChunks...)
	s.mu.Unlock()
	if start < 0 {
		start = 0
	}
	if start >= len(chunks) {
		return
	}
	// The stream starts fresh: the key that triggered it may have arrived
	// while other audio was playing, and that interruption must not stop
	// this stream before its first chunk.
	s.mu.Lock()
	sess.playInterrupted = false
	sess.BodyPaused = false
	sess.BodyInterrupted = false
	s.mu.Unlock()
	runtime := s.promptRuntime(sess)
	if runtime == nil {
		for i := start; i < len(chunks); i++ {
			s.prompt(sess, chunks[i])
			s.mu.Lock()
			sess.BodyIndex = i + 1
			s.mu.Unlock()
		}
	} else {
		dir := s.Media.Dir
		if dir == "" {
			dir = filepath.Dir(sess.TxPath)
		}
		s.streamSpeechChunksTracked(sess, dir, chunks[start:], func(index int) {
			s.mu.Lock()
			sess.BodyIndex = start + index + 1
			s.mu.Unlock()
		}, func(ctx context.Context, text, output string) error {
			s.mu.Lock()
			speed := sess.BodySpeed
			s.mu.Unlock()
			if speed != 0 {
				return runtime.SynthesizeAt(ctx, text, output, speed)
			}
			return runtime.Synthesize(ctx, text, output)
		}, func(ctx context.Context, path string) error {
			return s.Media.playWAVTail(ctx, sess.TxPath, path, pcmChunkDrainTail)
		})
	}
	if s.wasInterrupted(sess) {
		return
	}
	// A short body interrupted mid-prompt has not spent its unread marker
	// yet; finishing the resumed playback spends it.
	s.mu.Lock()
	markRead := sess.BodyMarkRead
	s.mu.Unlock()
	if markRead != nil {
		s.mu.Lock()
		sess.BodyMarkRead = nil
		s.mu.Unlock()
		markRead()
	}
}

// streamSpeechChunksTracked owns the double-buffered streaming loop, split
// from playSpeechChunks so tests can drive it with fake synthesis and
// playback. The staleness check re-reads playSequence after every chunk this
// loop plays: playPrompt advances the sequence for each chunk, so comparing
// against a pre-loop snapshot made the stream stop after its first chunk.
// onPlayed(index) fires after chunk index finished playing, and every early
// exit from an interruption marks the session so the caller can resume
// mid-message (pause, skip, speech rate).
func (s *Service) streamSpeechChunksTracked(sess *session, dir string, chunks []string, onPlayed func(int), synthesize func(context.Context, string, string) error, play func(context.Context, string) error) {
	s.mu.Lock()
	startSequence := sess.playSequence
	s.mu.Unlock()

	paths := make([]string, bodyChunkSlots)
	releases := make([]func(), bodyChunkSlots)
	for i := range paths {
		tmp, release, err := maintenance.CreateTemp(dir, ".body-chunk-*.wav")
		if err != nil {
			for j := 0; j < i; j++ {
				releases[j]()
				_ = os.Remove(paths[j])
			}
			return
		}
		paths[i] = tmp.Name()
		releases[i] = release
		_ = tmp.Close()
	}

	workerCtx, cancel := context.WithCancel(s.eventContext(sess))
	var worker sync.WaitGroup
	worker.Add(1)
	type produced struct {
		slot int
		err  error
	}
	free := make(chan int, bodyChunkSlots)
	ready := make(chan produced, bodyChunkSlots)
	for i := range paths {
		free <- i
	}
	go func() {
		defer worker.Done()
		defer close(ready)
		for _, chunk := range chunks {
			var slot int
			select {
			case slot = <-free:
			case <-workerCtx.Done():
				return
			}
			err := synthesize(workerCtx, chunk, paths[slot])
			select {
			case ready <- produced{slot: slot, err: err}:
			case <-workerCtx.Done():
				return
			}
		}
	}()
	defer func() {
		cancel()
		worker.Wait()
		for i := range paths {
			releases[i]()
			_ = os.Remove(paths[i])
		}
	}()

	markInterrupted := func() {
		s.mu.Lock()
		sess.BodyInterrupted = true
		s.mu.Unlock()
	}
	for i := range chunks {
		s.mu.Lock()
		stale := sess.Closed || sess.playSequence != startSequence || sess.playInterrupted
		s.mu.Unlock()
		if stale {
			markInterrupted()
			return
		}
		var item produced
		var ok bool
		select {
		case item, ok = <-ready:
			if !ok {
				return
			}
		case <-workerCtx.Done():
			return
		}
		if item.err != nil {
			free <- item.slot
			if s.Log != nil {
				s.Log.Warn("email body chunk synthesis failed", "error", item.err)
			}
			// A failed chunk used to end the rest of the body in silence;
			// say so before the menu follows.
			s.prompt(sess, "The rest of this message could not be read.")
			return
		}
		err := s.playPrompt(sess, func(playCtx context.Context) error {
			return play(playCtx, paths[item.slot])
		})
		// The slot returns only after its chunk finished playing. Handing it
		// back earlier let the worker reuse the file while its audio was
		// still on the line, so a long email could play the wrong chunk.
		free <- item.slot
		if err == nil && onPlayed != nil {
			onPlayed(i)
		}
		s.mu.Lock()
		// Adopt the sequence our own play just produced (or a newer prompt
		// replaced); the next iteration compares against the latest state,
		// not the pre-loop snapshot.
		startSequence = sess.playSequence
		s.mu.Unlock()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				markInterrupted()
			} else if s.Log != nil {
				s.Log.Warn("email body chunk playback failed", "error", err)
			}
			return
		}
	}
}
