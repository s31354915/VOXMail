package calls

import (
	"context"
	"os"
	"strings"
	"testing"
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
