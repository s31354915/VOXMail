package keypad

import "strings"

// MultiTap implements VOXMail's deterministic DTMF text editor.
type MultiTap struct {
	Text       string
	PendingKey byte
	Presses    int
	Mode       Mode
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
	'7': "p q r s 7",
	'8': "t u v 8",
	'9': "w x y z 9",
	'0': "0 ",
}

func New(mode Mode) *MultiTap { return &MultiTap{Mode: mode} }

// Press consumes one DTMF key. It returns committed text, if any, and whether
// the field is complete. A single star commits; two stars backspace.
func (m *MultiTap) Press(key byte) (committed string, done bool) {
	switch key {
	case '*':
		if m.PendingKey == 0 {
			m.Text = backspace(m.Text)
			return "", false
		}
		committed = m.current()
		m.Text += committed
		m.PendingKey, m.Presses = 0, 0
		return committed, false
	case '#':
		if m.PendingKey != 0 {
			committed = m.current()
			m.Text += committed
			m.PendingKey, m.Presses = 0, 0
		}
		return committed, true
	}
	if _, ok := groups[key]; !ok {
		return "", false
	}
	if m.PendingKey != key {
		if m.PendingKey != 0 {
			m.Text += m.current()
		}
		m.PendingKey, m.Presses = key, 0
	}
	m.Presses++
	return "", false
}

func (m *MultiTap) current() string {
	values := strings.ReplaceAll(groups[m.PendingKey], " ", "")
	return string(values[(m.Presses-1)%len(values)])
}

func backspace(value string) string {
	if value == "" {
		return value
	}
	runes := []rune(value)
	return string(runes[:len(runes)-1])
}
