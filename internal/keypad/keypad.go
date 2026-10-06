package keypad

import "unicode/utf8"

// MultiTap implements VOXMail's deterministic DTMF text editor.
type MultiTap struct {
	Text       string
	PendingKey byte
	Presses    int
	Mode       Mode
	// MaxRunes bounds the committed field. A zero value preserves the
	// historical unbounded behavior for generic callers.
	MaxRunes int
	// Overflowed reports input rejected at the configured boundary. It is
	// cleared when a backspace makes room or when a new editor is created.
	Overflowed bool
}

type Mode int

const (
	ModeText Mode = iota
	ModeEmail
)

var groups = map[byte]string{
	'1': "1@.?&+-_=/:;,$%()!#*'\"",
	'2': "abc2",
	'3': "def3",
	'4': "ghi4",
	'5': "jkl5",
	'6': "mno6",
	'7': "pqrs7",
	'8': "tuv8",
	'9': "wxyz9",
	'0': "0 ",
}

func New(mode Mode) *MultiTap { return &MultiTap{Mode: mode} }

// NewWithLimit creates an editor with a maximum committed Unicode-rune count.
// Limits are applied at commit boundaries so a pending multi-tap character is
// never silently written past the field boundary.
func NewWithLimit(mode Mode, maxRunes int) *MultiTap {
	if maxRunes < 0 {
		maxRunes = 0
	}
	return &MultiTap{Mode: mode, MaxRunes: maxRunes}
}

// Press consumes one DTMF key. It returns committed text, if any, and whether
// the field is complete. A single star commits; two stars backspace.
func (m *MultiTap) Press(key byte) (committed string, done bool) {
	switch key {
	case '*':
		if m.PendingKey == 0 {
			m.Text = backspace(m.Text)
			m.Overflowed = false
			return "", false
		}
		committed = m.appendCurrent()
		m.PendingKey, m.Presses = 0, 0
		return committed, false
	case '#':
		if m.PendingKey != 0 {
			committed = m.appendCurrent()
			m.PendingKey, m.Presses = 0, 0
		}
		if m.MaxRunes > 0 && utf8.RuneCountInString(m.Text) > m.MaxRunes {
			m.Overflowed = true
		}
		return committed, true
	}
	if _, ok := groups[key]; !ok {
		return "", false
	}
	if m.PendingKey != key {
		if m.PendingKey != 0 {
			if m.appendCurrent() == "" && m.Overflowed {
				m.PendingKey, m.Presses = 0, 0
				return "", false
			}
		}
		m.PendingKey, m.Presses = key, 0
	}
	if m.MaxRunes > 0 && utf8.RuneCountInString(m.Text) >= m.MaxRunes {
		m.Overflowed = true
		m.PendingKey, m.Presses = 0, 0
		return "", false
	}
	m.Presses++
	return "", false
}

func (m *MultiTap) appendCurrent() string {
	if m.PendingKey == 0 {
		return ""
	}
	committed := m.current()
	if m.MaxRunes > 0 && utf8.RuneCountInString(m.Text)+utf8.RuneCountInString(committed) > m.MaxRunes {
		m.Overflowed = true
		return ""
	}
	m.Text += committed
	return committed
}

func (m *MultiTap) current() string {
	values := groups[m.PendingKey]
	return string(values[(m.Presses-1)%len(values)])
}

func backspace(value string) string {
	if value == "" {
		return value
	}
	runes := []rune(value)
	return string(runes[:len(runes)-1])
}
