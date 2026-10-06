package mailparse

import (
	"context"
	"fmt"
	"io"
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
	err := scanContext(context.Background(), root, Parse, func(message MaildirMessage) error {
		result = append(result, message)
		return nil
	})
	return result, err
}

// ScanMetadata is a collecting compatibility helper. Use ScanMetadataEach for
// bounded-memory indexing and retention; it keeps no decoded attachment data.
func ScanMetadata(root string) ([]MaildirMessage, error) {
	var result []MaildirMessage
	err := scanContext(context.Background(), root, ParseMetadata, func(message MaildirMessage) error {
		result = append(result, message)
		return nil
	})
	return result, err
}

// ScanMetadataEach visits messages one at a time. The callback must not retain
// the message's attachment data; metadata scans intentionally omit it.
func ScanMetadataEach(root string, visit func(MaildirMessage) error) error {
	return scanContext(context.Background(), root, ParseMetadata, visit)
}

func ScanMetadataEachContext(ctx context.Context, root string, visit func(MaildirMessage) error) error {
	return scanContext(ctx, root, ParseMetadata, visit)
}

// ScanMetadataEachContextWithFilter visits metadata messages one at a time,
// allowing the caller to skip parsing when it can prove that a path has not
// changed. The filter runs after Walk has obtained the file's FileInfo and
// before the file is opened. A false result still means the path was observed
// by the scan; the caller remains responsible for recording that observation.
func ScanMetadataEachContextWithFilter(ctx context.Context, root string, shouldParse func(path string, info os.FileInfo) (bool, error), visit func(MaildirMessage) error) error {
	return scanContextWithFilter(ctx, root, ParseMetadata, shouldParse, visit)
}

func scanContext(ctx context.Context, root string, parser func(io.Reader) (Message, error), visit func(MaildirMessage) error) error {
	return scanContextWithFilter(ctx, root, parser, nil, visit)
}

func scanContextWithFilter(ctx context.Context, root string, parser func(io.Reader) (Message, error), shouldParse func(path string, info os.FileInfo) (bool, error), visit func(MaildirMessage) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
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
		if shouldParse != nil {
			parse, filterErr := shouldParse(path, info)
			if filterErr != nil {
				return filterErr
			}
			if !parse {
				return nil
			}
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		msg, parseErr := parser(contextReader{ctx: ctx, reader: file})
		_ = file.Close()
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
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
		return visit(MaildirMessage{Path: path, Folder: folder, Read: read, Message: msg})
	})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}
