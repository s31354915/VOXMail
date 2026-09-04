package mailer

import (
	"strings"
	"testing"
)

func TestBuildMessage(t *testing.T) {
	message := string(BuildMessage("ada@example.com", "Ada", []string{"john@example.com"}, nil, nil, "Hello", "Body"))
	if !strings.Contains(message, "To: john@example.com") || !strings.Contains(message, "Subject: Hello") {
		t.Fatal(message)
	}
}
