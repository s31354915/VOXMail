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

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
