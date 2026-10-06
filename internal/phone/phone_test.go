package phone

import "testing"

func TestNormalizeSIPAndTelephoneForms(t *testing.T) {
	tests := map[string]string{
		" +15551212 ":                            "+15551212",
		"sip:+15551212@example.com":              "+15551212",
		"<sip:+15551212@example.com;user=phone>": "+15551212",
		"tel:+15551212;ext=9":                    "+15551212",
		"+15551212":                              "+15551212",
	}
	for input, want := range tests {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}
