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
	s.mu.Lock()
	startSequence := sess.playSequence
	s.mu.Unlock()

	dir := s.Media.Dir
	if dir == "" {
		dir = filepath.Dir(sess.TxPath)
	}
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
			err := runtime.Synthesize(workerCtx, chunk, paths[slot])
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

	for range chunks {
		s.mu.Lock()
		stale := sess.Closed || sess.playSequence != startSequence
		s.mu.Unlock()
		if stale {
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
		free <- item.slot
		if item.err != nil {
			if s.Log != nil {
				s.Log.Warn("email body chunk synthesis failed", "error", item.err)
			}
			return
		}
		err := s.playPrompt(sess, func(playCtx context.Context) error {
			return s.Media.playWAV(playCtx, sess.TxPath, paths[item.slot])
		})
		if err != nil {
			if !errors.Is(err, context.Canceled) && s.Log != nil {
				s.Log.Warn("email body chunk playback failed", "error", err)
			}
			return
		}
	}
}
