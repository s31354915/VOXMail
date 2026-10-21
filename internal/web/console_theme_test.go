package web

import "testing"

func TestConsoleStylesPath(t *testing.T) {
	cases := []struct {
		theme string
		want  string
	}{
		{"", "console/console.css"},
		{"dark", "console/console-dark.css"},
		{"DARK", "console/console-dark.css"},
		{"  midnight  ", "console/console.css"}, // valid name, no such stylesheet
		{"../secret", "console/console.css"},
		{"bad name", "console/console.css"},
		{"-lead", "console/console.css"},
		{"dark;drop", "console/console.css"},
	}
	for _, tc := range cases {
		t.Setenv("VOXMAIL_THEME", tc.theme)
		if got := consoleStylesPath(); got != tc.want {
			t.Errorf("VOXMAIL_THEME=%q: got %q, want %q", tc.theme, got, tc.want)
		}
	}
}
