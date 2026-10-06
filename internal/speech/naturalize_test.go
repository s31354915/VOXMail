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
