package speech

import "testing"

func TestEmailToSpeechCurrencyWithThousandsAndMinorUnits(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"$1,234.56", "one thousand two hundred thirty-four dollars and fifty-six cents"},
		{"$1,234.00", "one thousand two hundred thirty-four dollars"},
		{"€5.20", "five euros and twenty euro cents"},
		{"£2.50", "two pounds and fifty pence"},
		{"$99.99", "ninety-nine dollars and ninety-nine cents"},
		{"Total: $12,345,678.", "Total: twelve million three hundred forty-five thousand six hundred seventy-eight dollars."},
	} {
		if got := EmailToSpeech(tc.input); got != tc.want {
			t.Errorf("EmailToSpeech(%q)=%q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestEmailToSpeechSpeaksFiveToSevenDigitRunsDigitByDigit(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"ZIP 11219", "ZIP one one two one nine"},
		{"Order 4829104 confirmed", "Order four eight two nine one zero four confirmed"},
		{"Tracking 48291047 shipped", "Tracking four eight two nine one zero four seven shipped"},
	} {
		if got := EmailToSpeech(tc.input); got != tc.want {
			t.Errorf("EmailToSpeech(%q)=%q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestEmailToSpeechKeepsImplausibleDatesAsWritten(t *testing.T) {
	for _, input := range []string{"Due 13/25/2026.", "Due 2026-13-05."} {
		got := EmailToSpeech(input)
		if got == "Due ." || got == "Due." {
			t.Fatalf("implausible date was dropped: input=%q output=%q", input, got)
		}
	}
}

func TestParseDigitsRejectsOverflow(t *testing.T) {
	if _, err := parseDigits("12345678901234567890123"); err == nil {
		t.Fatal("overflowing digits should report an error")
	}
	if value, err := parseDigits("6125"); err != nil || value != 6125 {
		t.Fatalf("plain digits misread: %d %v", value, err)
	}
}
