package speech

import "testing"

func TestSpokenValuesPreserveMeaning(t *testing.T) {
	cases := []struct{ input, want string }{
		{"$1234.50", "one thousand two hundred thirty-four dollars and fifty cents"},
		{"$.50", "zero dollars and fifty cents"},
		{"$3.259", "three point two five nine dollars"},
		{"o'connor@example.com", "o apostrophe connor at example dot com"},
		{"555-0123", "five five five, zero one two three"},
		{"$1.01", "one dollar and one cent"},
		{"$ 12.5", "twelve dollars and fifty cents"},
		{"-$7.00", "minus seven dollars"},
		{"£0.01", "zero pounds and one penny"},
		{"USD 20", "twenty U S dollars"},
		{"₹250", "two hundred fifty rupees"},
		{"1,234.56", "one thousand two hundred thirty-four point five six"},
		{"1234.5678", "one thousand two hundred thirty-four point five six seven eight"},
		{"1,000,000,000,000", "one trillion"},
		{"9,223,372,036,854,775,807", "nine quintillion two hundred twenty-three quadrillion three hundred seventy-two trillion thirty-six billion eight hundred fifty-four million seven hundred seventy-five thousand eight hundred seven"},
		{"10,000,000,000,000,000,000", "one zero zero zero zero zero zero zero zero zero zero zero zero zero zero zero zero zero zero zero"},
		{"PIN 0012", "PIN zero zero one two"},
		{"Code 1234", "Code one two three four"},
		{"alex01+news@example.com", "alex zero one plus news at example dot com"},
		{"Sam <sam@example.com>", "Sam sam at example dot com"},
		{"Call +1 (212) 555-0123 ext. 004", "Call plus one, two one two, five five five, zero one two three, extension zero zero four"},
		{"Call (212) 555-0123.", "Call two one two, five five five, zero one two three."},
		{"Version 2.3.4", "Version two dot three dot four"},
		{"Meet at 3:00", "Meet at three o'clock"},
		{"Due 2026-10-01", "Due October first, two thousand twenty-six"},
		{"The 21st floor", "The twenty-first floor"},
		{"123 Main St.", "one hundred twenty-three Main Street."},
		{"-12.5°C", "minus twelve point five degrees Celsius"},
		{"2 + 3 = 5 & 4 < 6", "two plus three equals five and four less than six"},
		{"Press @, #, or *.", "Press at, hash, or asterisk."},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := EmailToSpeech(tc.input)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if again := EmailToSpeech(got); again != got {
				t.Errorf("second normalization changed %q to %q", got, again)
			}
		})
	}
}

func TestCharacterReadbackNamesSymbolsAndZeros(t *testing.T) {
	if got := SpellCharacters("a01+_@b.co"); got != "A, zero, one, plus, underscore, at, B, dot, C, O" {
		t.Fatalf("ambiguous readback: %q", got)
	}
}
