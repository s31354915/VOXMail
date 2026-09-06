package mailconfig

import (
	"fmt"
	"net/url"
	"sort"
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
	FolderMap   map[string]string
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
	for remote, local := range a.FolderMap {
		if strings.TrimSpace(remote) == "" || strings.ContainsAny(remote, "\x00\r\n") {
			return "", fmt.Errorf("invalid remote folder mapping")
		}
		if local == "" || strings.ContainsAny(local, "\x00\r\n") || strings.HasPrefix(local, "/") || strings.Contains(local, "..") {
			return "", fmt.Errorf("invalid local folder mapping for %q", remote)
		}
	}
	name := safe(a.ID)
	root := strings.TrimRight(a.MaildirRoot, "/") + "/"
	inbox := root + "Inbox"
	for remote, local := range a.FolderMap {
		if strings.EqualFold(remote, "INBOX") && local != "" {
			inbox = root + strings.Trim(local, "/")
			break
		}
	}
	exclusions := make([]string, 0, len(a.FolderMap))
	for remote := range a.FolderMap {
		if !strings.EqualFold(remote, "INBOX") && remote != "" {
			exclusions = append(exclusions, "!"+quote(remote))
		}
	}
	patterns := "*"
	if len(exclusions) > 0 {
		sort.Strings(exclusions)
		patterns += " " + strings.Join(exclusions, " ")
	}
	remotes := make([]string, 0, len(a.FolderMap))
	for remote := range a.FolderMap {
		remotes = append(remotes, remote)
	}
	sort.Strings(remotes)
	base := fmt.Sprintf(`IMAPAccount %s
Host %s
Port %d
User %s
PassCmd "/usr/local/bin/voxmail-secret %s"
SSLType IMAPS

IMAPStore %s-remote
Account %s

MaildirStore %s-local
Path %s
Inbox %s
SubFolders Verbatim

Channel %s
Master :%s-remote:
Slave :%s-local:
Patterns %s
Create Slave
Remove None
Sync All
Expunge None
SyncState *
	`, name, quote(a.IMAPHost), a.IMAPPort, quote(a.IMAPUser), name, name, name, name, quote(root), quote(inbox), name, name, name, patterns)
	for _, remote := range remotes {
		local := a.FolderMap[remote]
		if strings.EqualFold(remote, "INBOX") || remote == "" || local == "" {
			continue
		}
		channel := safe(name + "-" + remote)
		base += fmt.Sprintf(`
Channel %s
Master :%s-remote:%s
Slave :%s-local:%s
Create Slave
Remove None
Sync All
Expunge None
SyncState *
`, channel, name, quote(remote), name, quote(local))
	}
	return base, nil
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
