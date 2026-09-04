package keypad

import "testing"

func TestMultiTap(t *testing.T) {
	m := New(ModeText)
	for _, key := range []byte("2*22*222#") {
		_, done := m.Press(key)
		if key == '#' && !done {
			t.Fatal("# did not finish entry")
		}
	}
	if m.Text != "abc" {
		t.Fatalf("got %q, want abc", m.Text)
	}
}

func TestBackspace(t *testing.T) {
	m := New(ModeText)
	m.Text = "abc"
	m.Press('*')
	if m.Text != "ab" {
		t.Fatalf("got %q, want ab", m.Text)
	}
}
