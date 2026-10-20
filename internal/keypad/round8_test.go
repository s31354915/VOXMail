package keypad

import "testing"

func TestBackspaceClearsOverflowPromptState(t *testing.T) {
	m := NewWithLimit(ModeText, 3)
	m.Text = "abc"
	if _, done := m.Press('2'); done || !m.Overflowed {
		t.Fatalf("expected overflow at the limit, got overflowed=%v done=%v", m.Overflowed, done)
	}
	m.OverflowPrompted = true
	if _, done := m.Press('*'); done || !m.Overflowed || !m.OverflowPrompted {
		t.Fatalf("a single star should do nothing, got overflowed=%v prompted=%v", m.Overflowed, m.OverflowPrompted)
	}
	if _, done := m.Press('*'); done || m.Overflowed || m.OverflowPrompted {
		t.Fatalf("double star should clear overflow state, got overflowed=%v prompted=%v", m.Overflowed, m.OverflowPrompted)
	}
	if _, done := m.Press('2'); done || m.Overflowed {
		t.Fatalf("typing should resume after backspace, got overflowed=%v done=%v", m.Overflowed, done)
	}
	if m.PendingKey != '2' || m.Presses != 1 {
		t.Fatalf("resume did not accept the key: pending=%q presses=%d", m.PendingKey, m.Presses)
	}
}
