package mailsecurity

import "testing"

func TestNormalizeIMAPCanonicalAndLegacyValues(t *testing.T) {
	tests := []struct {
		name, value string
		port        int
		want        string
	}{
		{name: "implicit default", port: 993, want: ImplicitTLS},
		{name: "starttls default", port: 143, want: StartTLS},
		{name: "canonical implicit", value: "IMPLICIT_TLS", port: 993, want: ImplicitTLS},
		{name: "legacy imaps", value: "IMAPS", port: 993, want: ImplicitTLS},
		{name: "canonical starttls", value: "STARTTLS", port: 143, want: StartTLS},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeIMAP(test.value, test.port)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("NormalizeIMAP(%q,%d)=%q, want %q", test.value, test.port, got, test.want)
			}
		})
	}
}

func TestNormalizeRejectsUnsupportedOrPlaintext(t *testing.T) {
	for _, value := range []string{"plaintext", "none", "tls1.3"} {
		if _, err := NormalizeIMAP(value, 993); err == nil {
			t.Fatalf("NormalizeIMAP accepted %q", value)
		}
		if _, err := NormalizeSMTP(value, 465); err == nil {
			t.Fatalf("NormalizeSMTP accepted %q", value)
		}
	}
}

func TestNormalizeSMTPDoesNotAcceptIMAPLegacySpelling(t *testing.T) {
	if _, err := NormalizeSMTP("IMAPS", 465); err == nil {
		t.Fatal("NormalizeSMTP accepted IMAPS")
	}
}
