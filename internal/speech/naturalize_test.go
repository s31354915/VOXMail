package speech

import "testing"

func TestEmailToSpeech(t *testing.T) {
	got := EmailToSpeech(`<p>Call +1 (555) 123-4567 or email a.user@example.com.</p><a href="https://example.com/x">read</a> 25%`)
	for _, want := range []string{"plus", "at", "dot", "percent", "link"} {
		if !contains(got, want) {
			t.Fatalf("%q missing %q", got, want)
		}
	}
}

func TestEmailToSpeechUnicodeCurrency(t *testing.T) {
	for _, tc := range []struct {
		input, want string
	}{
		{"€12.50", "12 euros and 50 cents"},
		{"£7", "7 pounds"},
		{"$3.25", "3 dollars and 25 cents"},
	} {
		if got := EmailToSpeech(tc.input); got != tc.want {
			t.Fatalf("EmailToSpeech(%q)=%q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeForSpeechUsesNaturalApplicationPronunciation(t *testing.T) {
	for _, tc := range []struct {
		input, want string
	}{
		{"Welcome to VOXMail. Please enter your PIN, then press pound.", "Welcome to Vox Mail. Please enter your pin, then press pound."},
		{"VOXMail reads synchronized email over SIP. Use IMAP and SMTP.", "Vox Mail reads synchronized email over sip. Use eye map and S M T P."},
		{"Open https://example.com and email a.user@example.com", "Open link and email a dot user at example dot com"},
		{"IMPORTANT: this is ordinary all-capital prose.", "IMPORTANT: this is ordinary all-capital prose."},
	} {
		if got := NormalizeForSpeech(tc.input); got != tc.want {
			t.Errorf("NormalizeForSpeech(%q)=%q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeForSpeechIsIdempotent(t *testing.T) {
	input := "Welcome to VOXMail. Your PIN uses IMAP over HTTPS."
	first := NormalizeForSpeech(input)
	if got := NormalizeForSpeech(first); got != first {
		t.Fatalf("normalization is not idempotent: first=%q, second=%q", first, got)
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func TestEmailToSpeechBlockTagsBreakSentences(t *testing.T) {
	got := EmailToSpeech("<p>First paragraph.</p><p>Second paragraph.</p><br>Third line.")
	if got != "First paragraph. Second paragraph. Third line." {
		t.Fatalf("EmailToSpeech paragraphs = %q", got)
	}
}

func TestEmailToSpeechDatesAreSpokenNaturally(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Meeting moved to 2026-10-17.", "Meeting moved to October 17, 2026."},
		{"Due 10/17/2026 at noon.", "Due October 17, 2026 at noon."},
		{"Order 48291047 shipped.", "Order 4 8 2 9 1 0 4 7 shipped."},
		{"Version 2.3.4 shipped.", "Version 2.3.4 shipped."},
	} {
		if got := EmailToSpeech(tc.input); got != tc.want {
			t.Errorf("EmailToSpeech(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestEmailToSpeechKeepsParagraphBreaks(t *testing.T) {
	got := EmailToSpeech("Hi there,\n\nThanks for the update.\n\nBest,\nAlex")
	want := "Hi there, Thanks for the update. Best, Alex"
	if got != want {
		t.Fatalf("EmailToSpeech=%q, want %q", got, want)
	}
	if got := EmailToSpeech("line one\nline two"); got != "line one line two" {
		t.Fatalf("hard-wrapped newline collapsed wrongly: %q", got)
	}
	if got := EmailToSpeech("> old reply\n\nFresh text"); got != "old reply. Fresh text" {
		t.Fatalf("quoted marker not stripped: %q", got)
	}
	if again := EmailToSpeech(got); again != got {
		t.Fatalf("EmailToSpeech is not idempotent on paragraph output: %q", again)
	}
}
