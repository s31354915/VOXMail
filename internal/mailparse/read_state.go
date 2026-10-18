package mailparse

import (
	"errors"
	"os"
	"strings"
)

// setMaildirRead updates the Maildir info suffix without touching the message
// body. Maildir flags are the local cache's durable read-state representation;
// changing SQLite alone would be overwritten by the next index pass.
func SetMaildirRead(path string, read bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("maildir path is empty")
	}
	target := MaildirReadPath(path, read)
	if target == path {
		return path, nil
	}
	if err := os.Rename(path, target); err != nil {
		return "", err
	}
	return target, nil
}

// MaildirReadPath returns the path the message would carry after
// SetMaildirRead, without renaming anything on disk. Callers use it to
// re-derive the current on-disk name of a message whose listed path is stale
// because an earlier flag change renamed the file.
func MaildirReadPath(path string, read bool) string {
	if strings.TrimSpace(path) == "" {
		return path
	}
	marker := ":2,"
	base, flags := path, ""
	if index := strings.LastIndex(path, marker); index >= 0 {
		base, flags = path[:index], path[index+len(marker):]
	}
	var kept strings.Builder
	for _, flag := range flags {
		if flag != 'S' && flag != 's' {
			kept.WriteRune(flag)
		}
	}
	flags = kept.String()
	if read {
		flags += "S"
	}
	target := base + marker + flags
	if target == path {
		return path
	}
	return target
}
