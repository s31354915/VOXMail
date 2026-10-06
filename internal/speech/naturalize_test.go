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

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
