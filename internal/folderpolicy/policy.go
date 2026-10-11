// Package folderpolicy describes explicit owner-selected logical mailboxes.
// Physical remote folders and their Maildirs remain separate: a group is a
// presentation/query choice, never a shared mbsync UID-state directory.
package folderpolicy

import (
	"fmt"
	"strings"
)

// Group preserves remote names verbatim. Primary is the remote destination for
// operations targeting this logical mailbox; reads include every member.
type Group struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Primary string   `json:"primary"`
	Folders []string `json:"folders"`
}

// Policy is empty by default. No provider/name-based equivalence is inferred.
// Exclusions suppress selected folders from logical browsing without deleting
// their remote data. Synchronization/storage enforce their own physical policy.
type Policy struct {
	Groups   []Group  `json:"groups"`
	Excluded []string `json:"excluded"`
}

// Key cannot collide with physical folder names, which must be relative paths.
func (g Group) Key() string { return "/groups/" + g.ID }

func validName(value string) bool {
	return value != "" && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n") && len(value) <= 1024
}

// RemoteKey normalizes only the protocol-reserved, case-insensitive INBOX name.
// All other remote mailbox names retain their exact identity.
func RemoteKey(value string) string {
	// IMAP reserves INBOX as case-insensitive. Other mailbox names retain case.
	if strings.EqualFold(value, "INBOX") {
		return "INBOX"
	}
	return value
}

// Validate checks an entire owner selection before it can be persisted.
// available must contain the remote names discovered/retained for this account.
func (p Policy) Validate(available []string) error {
	if len(p.Groups) > 256 || len(p.Excluded) > 4096 {
		return fmt.Errorf("too many folder selections")
	}
	known := make(map[string]bool, len(available))
	for _, folder := range available {
		known[RemoteKey(folder)] = true
	}
	claimed := make(map[string]bool)
	claim := func(folder string) error {
		key := RemoteKey(folder)
		if !validName(folder) || !known[key] {
			return fmt.Errorf("folder %q is not available in this account", folder)
		}
		if claimed[key] {
			return fmt.Errorf("folder %q is selected more than once", folder)
		}
		claimed[key] = true
		return nil
	}
	ids, names := make(map[string]bool), make(map[string]bool)
	for _, group := range p.Groups {
		if len(group.ID) == 0 || len(group.ID) > 64 || strings.IndexFunc(group.ID, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
		}) >= 0 || ids[group.ID] {
			return fmt.Errorf("invalid or repeated group identity")
		}
		if !validName(group.Name) || names[group.Name] {
			return fmt.Errorf("invalid or repeated group name")
		}
		ids[group.ID], names[group.Name] = true, true
		if len(group.Folders) < 2 || len(group.Folders) > 4096 {
			return fmt.Errorf("group %q must contain at least two folders", group.Name)
		}
		primaryFound := false
		for _, folder := range group.Folders {
			if err := claim(folder); err != nil {
				return err
			}
			primaryFound = primaryFound || RemoteKey(folder) == RemoteKey(group.Primary)
		}
		if !validName(group.Primary) || !primaryFound {
			return fmt.Errorf("group %q requires an explicitly selected member as primary", group.Name)
		}
	}
	for _, folder := range p.Excluded {
		if err := claim(folder); err != nil {
			return err
		}
	}
	return nil
}

// Membership returns a group's stable logical key or an exclusion flag for a
// remote folder. Unselected folders retain independent visibility.
func (p Policy) Membership(remote string) (key string, excluded bool) {
	for _, folder := range p.Excluded {
		if RemoteKey(folder) == RemoteKey(remote) {
			return "", true
		}
	}
	for _, group := range p.Groups {
		for _, folder := range group.Folders {
			if RemoteKey(folder) == RemoteKey(remote) {
				return group.Key(), false
			}
		}
	}
	return "", false
}

func (p Policy) GroupForKey(key string) (Group, bool) {
	for _, group := range p.Groups {
		if group.Key() == key {
			return group, true
		}
	}
	return Group{}, false
}
