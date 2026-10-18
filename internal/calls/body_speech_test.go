package calls

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSplitSpeechChunksGroupsSentencesUnderLimit(t *testing.T) {
	text := "First sentence here. Second sentence follows. Third one is short. Fourth."
	chunks := splitSpeechChunks(text, 60)
	if len(chunks) == 0 {
		t.Fatal("expected chunks")
	}
	for _, chunk := range chunks {
		if runeLen(chunk) > 60 {
			t.Fatalf("chunk exceeds limit: %q", chunk)
		}
	}
	joined := strings.Join(chunks, " ")
	if joined != text {
		t.Fatalf("chunks lost text: got %q, want %q", joined, text)
	}
}

func TestSplitSpeechChunksSplitsLongSentenceAtWord(t *testing.T) {
	long := strings.Repeat("word ", 40)
	chunks := splitSpeechChunks(long, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected long text to split, got %d chunk(s)", len(chunks))
	}
	for _, chunk := range chunks {
		if runeLen(chunk) > 100 {
			t.Fatalf("chunk exceeds limit: %q", chunk)
		}
		if strings.Contains(chunk, "wordword") {
			t.Fatalf("chunk split mid-word: %q", chunk)
		}
	}
}

func TestSplitSpeechChunksNewlinesAreBoundaries(t *testing.T) {
	// With a small limit each line stays its own chunk; with room to merge
	// they belong in one chunk, which TestSplitSpeechChunksGroupsSentences
	// covers.
	chunks := splitSpeechChunks("Line one.\nLine two.\nLine three.", 15)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks from newlines, got %d: %#v", len(chunks), chunks)
	}
}

func TestTruncateForSpeechKeepsSentenceBoundary(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 200; i++ {
		body.WriteString("Sentence number something here. ")
	}
	truncated, ok := truncateForSpeech(body.String(), 200)
	if !ok {
		t.Fatal("expected truncation")
	}
	if runeLen(truncated) > 200 {
		t.Fatalf("truncated text exceeds limit: %d", runeLen(truncated))
	}
	last := truncated[len(truncated)-1]
	if last != '.' && last != '!' && last != '?' {
		t.Fatalf("truncation did not end at a sentence boundary: %q", truncated)
	}
}

func TestTruncateForSpeechFallsBackToWordBoundary(t *testing.T) {
	body := strings.Repeat("word ", 60)
	truncated, ok := truncateForSpeech(body, 50)
	if !ok {
		t.Fatal("expected truncation")
	}
	if runeLen(truncated) > 50 {
		t.Fatalf("truncated text exceeds limit: %d", runeLen(truncated))
	}
	if strings.HasSuffix(strings.TrimSpace(truncated), "wor") {
		t.Fatalf("truncation split a word: %q", truncated)
	}
	if strings.HasSuffix(truncated, " ") {
		t.Fatalf("truncation kept trailing space: %q", truncated)
	}
}

func TestTruncateForSpeechPassthrough(t *testing.T) {
	text := "Short body."
	if truncated, ok := truncateForSpeech(text, 50); ok || truncated != text {
		t.Fatalf("short text should pass through unchanged: ok=%v got=%q", ok, truncated)
	}
}

func TestStreamSpeechChunksPlaysEveryChunk(t *testing.T) {
	svc := &Service{sessions: make(map[string]*session)}
	sess := &session{CallID: "stream"}
	svc.sessions[sess.CallID] = sess
	plays := 0
	play := func(ctx context.Context, path string) error {
		svc.mu.Lock()
		sess.playSequence++
		svc.mu.Unlock()
		plays++
		return nil
	}
	synthesize := func(ctx context.Context, text, output string) error {
		return os.WriteFile(output, []byte("wav"), 0600)
	}
	svc.streamSpeechChunks(sess, t.TempDir(), []string{"one", "two", "three", "four"}, synthesize, play)
	if plays != 4 {
		t.Fatalf("played %d of 4 chunks; the stream stopped early", plays)
	}
}

// A chunk's buffer slot must stay reserved until its playback ends. Handing
// the slot back early let the worker synthesize a later chunk into the same
// file while that chunk's audio was still on the line.
func TestStreamSpeechChunksKeepsSlotUntilPlaybackEnds(t *testing.T) {
	svc := &Service{sessions: make(map[string]*session)}
	sess := &session{CallID: "stream-slot"}
	svc.sessions[sess.CallID] = sess

	var mu sync.Mutex
	var payloads []string
	written := make(chan string, 8)
	playStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	synthesize := func(ctx context.Context, text, output string) error {
		if err := os.WriteFile(output, []byte(text), 0600); err != nil {
			return err
		}
		written <- text
		return nil
	}
	play := func(ctx context.Context, path string) error {
		mu.Lock()
		first := len(payloads) == 0
		mu.Unlock()
		if first {
			playStarted <- struct{}{}
			<-release
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mu.Lock()
		payloads = append(payloads, string(data))
		mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.streamSpeechChunks(sess, t.TempDir(), []string{"one", "two", "three", "four"}, synthesize, play)
	}()
	<-playStarted
	// While the first chunk holds its slot, a later chunk must not be
	// synthesized into it. A buggy implementation writes "three" here.
	overwrote := false
	timeout := time.After(300 * time.Millisecond)
waiting:
	for {
		select {
		case text := <-written:
			if text == "three" {
				overwrote = true
				break waiting
			}
		case <-timeout:
			break waiting
		}
	}
	close(release)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if overwrote {
		t.Fatalf("a later chunk was synthesized while the first chunk was still playing")
	}
	want := []string{"one", "two", "three", "four"}
	if len(payloads) != len(want) {
		t.Fatalf("played %d chunks, want %d", len(payloads), len(want))
	}
	for i, chunk := range want {
		if payloads[i] != chunk {
			t.Fatalf("chunk %d played %q, want %q", i, payloads[i], chunk)
		}
	}
}
