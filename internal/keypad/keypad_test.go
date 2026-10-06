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

func TestZeroKeyCanEnterSpace(t *testing.T) {
	m := New(ModeText)
	for _, key := range []byte("00#") {
		m.Press(key)
	}
	if m.Text != " " {
		t.Fatalf("got %q, want a space", m.Text)
	}
}

func TestMultiTapLimitRejectsOverflowAndBackspaceRecovers(t *testing.T) {
	m := NewWithLimit(ModeText, 1)
	for _, key := range []byte("2#") {
		m.Press(key)
	}
	if m.Text != "a" || m.Overflowed {
		t.Fatalf("initial value=%q overflow=%v, want a non-overflowing one-rune value", m.Text, m.Overflowed)
	}
	if done := func() bool { _, done := m.Press('3'); return done }(); done {
		t.Fatal("non-terminal key unexpectedly completed the field")
	}
	if !m.Overflowed || m.Text != "a" {
		t.Fatalf("overflow state=%q,%v, want unchanged text with overflow", m.Text, m.Overflowed)
	}
	m.Press('*')
	if m.Overflowed || m.Text != "" {
		t.Fatalf("backspace did not recover editor: text=%q overflow=%v", m.Text, m.Overflowed)
	}
	for _, key := range []byte("3#") {
		m.Press(key)
	}
	if m.Text != "d" {
		t.Fatalf("recovered editor text=%q, want d", m.Text)
	}
}

func TestMultiTapLimitCountsRunes(t *testing.T) {
	m := NewWithLimit(ModeText, 1)
	m.Text = "é"
	m.Press('2')
	if !m.Overflowed || m.Text != "é" {
		t.Fatalf("rune limit was not enforced: text=%q overflow=%v", m.Text, m.Overflowed)
	}
}

func TestMultiTapLimitRejectsOverlongSeedOnCommit(t *testing.T) {
	m := NewWithLimit(ModeText, 1)
	m.Text = "ab"
	_, done := m.Press('#')
	if !done || !m.Overflowed || m.Text != "ab" {
		t.Fatalf("overlong seed result=%q,%v,%v", m.Text, done, m.Overflowed)
	}
}
