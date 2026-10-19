package ivr

import (
	"strings"
	"testing"

	"github.com/voxmail/voxmail/internal/keypad"
)

func TestGuideExamplesMatchKeypadBehavior(t *testing.T) {
	for _, tc := range []struct{ keys, want string }{
		{"2*22#", "ab"},
		{"2222#", "2"},
		{"22222#", "a"},
		{"2*2#", "aa"},
		{"11#", "@"},
		{"111#", "."},
		{strings.Repeat("1", 8) + "#", "_"},
		{strings.Repeat("1", 19) + "#", "#"},
		{strings.Repeat("1", 20) + "#", "*"},
		{"2*00*22#", "a b"},
		{"2**#", ""},
		{"2*11*22*111*222*666*6#", "a@b.com"},
	} {
		editor := keypad.New(keypad.ModeEmail)
		for _, key := range []byte(tc.keys) {
			editor.Press(key)
		}
		if editor.Text != tc.want {
			t.Errorf("guide example %s produces %q, want %q", tc.keys, editor.Text, tc.want)
		}
	}
}

func TestEveryHelpPageIsReachableAndReturnsToItsParent(t *testing.T) {
	seen := map[State]bool{}
	var visit func(State)
	visit = func(state State) {
		if seen[state] {
			return
		}
		seen[state] = true
		page := helpPages[state]
		if _, ok := Spec(page.back); !ok {
			t.Errorf("%s has an unreachable back target %s", state, page.back)
		}
		for _, child := range page.children {
			if !IsHelpState(child) {
				t.Errorf("%s references missing page %s", state, child)
				continue
			}
			visit(child)
		}
	}
	visit("info")
	for state := range helpPages {
		if !seen[state] {
			t.Errorf("help page %s cannot be reached", state)
		}
	}
}
