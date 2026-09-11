package mailparse

import (
	"os"
	"path/filepath"
	"strings"
)

type MaildirMessage struct {
	Path   string
	Folder string
	Read   bool
	Message
}

func Scan(root string) ([]MaildirMessage, error) {
	var result []MaildirMessage
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		parent := filepath.Base(filepath.Dir(path))
		if parent != "cur" && parent != "new" {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		msg, parseErr := Parse(file)
		_ = file.Close()
		if parseErr != nil {
			return nil
		}
		folderPath := filepath.Dir(filepath.Dir(path))
		folder, relErr := filepath.Rel(root, folderPath)
		if relErr != nil || folder == "." || strings.HasPrefix(folder, ".."+string(filepath.Separator)) {
			return nil
		}
		folder = filepath.ToSlash(folder)
		name := filepath.Base(path)
		read := false
		if marker := strings.Index(name, ":2,"); marker >= 0 {
			read = strings.Contains(name[marker+3:], "S")
		}
		result = append(result, MaildirMessage{Path: path, Folder: folder, Read: read, Message: msg})
		return nil
	})
	return result, err
}
