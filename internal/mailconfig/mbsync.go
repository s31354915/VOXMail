package mailconfig

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Account is the non-secret portion needed to generate an mbsync channel.
type Account struct {
	ID          string
	IMAPHost    string
	IMAPPort    int
	IMAPUser    string
	MaildirRoot string
}

// Generate creates a channel that mirrors every existing remote folder while
// allowing only local folder creation. Mappings are deliberately not used as
// patterns: they are semantic aliases consumed by the application.
func Generate(a Account) (string, error) {
	if a.ID == "" || a.IMAPHost == "" || a.IMAPUser == "" || a.MaildirRoot == "" {
		return "", fmt.Errorf("account id, host, user, and maildir root are required")
	}
	if a.IMAPPort == 0 {
		a.IMAPPort = 993
	}
	name := safe(a.ID)
	root := strings.TrimRight(a.MaildirRoot, "/") + "/"
	return fmt.Sprintf(`IMAPAccount %s
Host %s
Port %d
User %s
PassCmd "/usr/local/bin/voxmail-secret %s"
SSLType IMAPS

IMAPStore %s-remote
Account %s

MaildirStore %s-local
Path %s
Inbox %sInbox
SubFolders Verbatim

Channel %s
Master :%s-remote:
Slave :%s-local:
Patterns *
Create Slave
Remove None
Sync All
Expunge None
SyncState *
	`, name, quote(a.IMAPHost), a.IMAPPort, quote(a.IMAPUser), name, name, name, name, quote(root), quote(root), name, name, name), nil
}

func safe(value string) string {
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, value)
	return value
}

func quote(value string) string {
	// mbsync's quoted values use C-style escapes. Rejecting control characters
	// avoids generating configuration with ambiguous lines.
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, value)
	return strconv.Quote(value)
}

// ValidateServerURL is used by onboarding before credentials are persisted.
func ValidateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "imaps" && u.Scheme != "imap") {
		return fmt.Errorf("invalid IMAP URL")
	}
	return nil
}
