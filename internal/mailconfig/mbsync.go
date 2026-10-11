package mailconfig

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/voxmail/voxmail/internal/mailsecurity"
	"github.com/voxmail/voxmail/internal/netguard"
)

// Account is the non-secret portion needed to generate an mbsync channel.
type Account struct {
	ID       string
	IMAPHost string
	// IMAPAddress is the already-validated numeric endpoint used by the
	// controlled tunnel helper. IMAPHost remains the TLS verification name.
	IMAPAddress  string
	IMAPPort     int
	IMAPSecurity string
	IMAPUser     string
	MaildirRoot  string
	FolderMap    map[string]string
	// RemoteFolders is an authenticated LIST snapshot. A non-nil snapshot
	// selects explicit pairs so retained local aliases cannot become remote names.
	RemoteFolders []string
}

// Generate mirrors provider folders while allowing only local folder creation.
// Production supplies an authenticated snapshot for explicit mailbox pairs;
// callers without a snapshot retain wildcard discovery for compatibility.
func Generate(a Account) (string, error) {
	if a.ID == "" || a.IMAPHost == "" || a.IMAPUser == "" || a.MaildirRoot == "" {
		return "", fmt.Errorf("account id, host, user, and maildir root are required")
	}
	if a.IMAPPort == 0 {
		a.IMAPPort = 993
	}
	security, err := mbsyncSecurity(a.IMAPSecurity, a.IMAPPort)
	if err != nil {
		return "", err
	}
	tunnel := ""
	if strings.TrimSpace(a.IMAPAddress) != "" {
		if err := netguard.ValidateAddress(a.IMAPAddress, a.IMAPPort, false); err != nil {
			return "", fmt.Errorf("invalid IMAP tunnel address: %w", err)
		}
		tunnel = fmt.Sprintf("Tunnel %s\n", quote("/usr/local/bin/voxmail-connect "+a.IMAPAddress))
	}
	for remote, local := range a.FolderMap {
		if strings.TrimSpace(remote) == "" || strings.ContainsAny(remote, "\x00\r\n") {
			return "", fmt.Errorf("invalid remote folder mapping")
		}
		if local == "" || strings.ContainsAny(local, "\x00\r\n") || strings.HasPrefix(local, "/") || strings.Contains(local, "..") {
			return "", fmt.Errorf("invalid local folder mapping for %q", remote)
		}
	}
	for _, remote := range a.RemoteFolders {
		if strings.TrimSpace(remote) == "" || strings.ContainsAny(remote, "\x00\r\n") || strings.HasPrefix(remote, "/") || strings.Contains(remote, "..") {
			return "", fmt.Errorf("invalid discovered remote folder")
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
	patternLine := "Patterns " + patterns + "\n"
	if a.RemoteFolders != nil {
		// An exact INBOX pattern also makes mbsync LIST the server and learn
		// its hierarchy delimiter before opening explicit nested pairs.
		patternLine = "Patterns INBOX\n"
	}
	remotes := mappedRemotes(a)
	base := fmt.Sprintf(`IMAPAccount %s
Host %s
Port %d
Timeout 60
%sUser %s
PassCmd "/usr/local/bin/voxmail-secret %s"
SSLType %s

IMAPStore %s-remote
Account %s

MaildirStore %s-local
Path %s
Inbox %s
SubFolders Verbatim

Channel %s
Master :%s-remote:
Slave :%s-local:
%sCreate Slave
Remove None
Sync All
Expunge None
SyncState *
	`, name, quote(a.IMAPHost), a.IMAPPort, tunnel, quote(a.IMAPUser), name, security, name, name, name, quote(root), quote(inbox), name, name, name, patternLine)
	for i, remote := range remotes {
		local := a.FolderMap[remote]
		if local == "" {
			local = remote
		}
		channel := safe(fmt.Sprintf("%s-folder-%d", name, i+1))
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

// mbsyncSecurity is the adapter between VOXMail's persisted transport names
// and mbsync's configuration vocabulary. Keep this translation at the
// boundary; passing the application name through would turn implicit_tls into
// IMPLICIT_TLS, which mbsync rejects.
func mbsyncSecurity(value string, port int) (string, error) {
	canonical, err := mailsecurity.NormalizeIMAP(value, port)
	if err != nil {
		return "", err
	}
	switch canonical {
	case mailsecurity.ImplicitTLS:
		return "IMAPS", nil
	case mailsecurity.StartTLS:
		return "STARTTLS", nil
	default:
		return "", fmt.Errorf("unsupported IMAP security mode %q", canonical)
	}
}

// ChannelNames returns the exact channels emitted by Generate. With a snapshot,
// the main channel selects INBOX and each discovered/mapped non-Inbox folder
// gets an explicit pair. Numeric suffixes avoid sanitized-name collisions.
func ChannelNames(a Account) []string {
	name := safe(a.ID)
	remotes := mappedRemotes(a)
	channels := make([]string, 0, len(remotes)+1)
	channels = append(channels, name)
	for i := range remotes {
		channels = append(channels, safe(fmt.Sprintf("%s-folder-%d", name, i+1)))
	}
	return channels
}

// LocalFolders identifies physical directories covered by the generated plan.
// nil represents the compatibility wildcard; production snapshots are explicit.
func LocalFolders(a Account) []string {
	if a.RemoteFolders == nil {
		return nil
	}
	inbox := "Inbox"
	for remote, local := range a.FolderMap {
		if strings.EqualFold(remote, "INBOX") && local != "" {
			inbox = local
			break
		}
	}
	selected := map[string]bool{path.Clean(inbox): true}
	for _, remote := range mappedRemotes(a) {
		local := a.FolderMap[remote]
		if local == "" {
			local = remote
		}
		selected[path.Clean(local)] = true
	}
	folders := make([]string, 0, len(selected))
	for folder := range selected {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	return folders
}

func mappedRemotes(a Account) []string {
	selected := make(map[string]bool)
	for remote, local := range a.FolderMap {
		if strings.EqualFold(remote, "INBOX") || strings.TrimSpace(remote) == "" || strings.TrimSpace(local) == "" {
			continue
		}
		selected[remote] = true
	}
	for _, remote := range a.RemoteFolders {
		if !strings.EqualFold(remote, "INBOX") && remote != "" {
			selected[remote] = true
		}
	}
	remotes := make([]string, 0, len(selected))
	for remote := range selected {
		remotes = append(remotes, remote)
	}
	sort.Strings(remotes)
	return remotes
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
