package store

import (
	"strings"
	"testing"
)

func FuzzCleanFolderRejectsTraversal(f *testing.F) {
	for _, seed := range []string{"Inbox", "Archive/Projects", "../outside", "Archive/../../outside", `Archive\\..\\outside`, "/absolute"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got := filepathCleanFolder(input)
		if got == "" || strings.HasPrefix(got, "/") || strings.ContainsAny(got, "\x00\r\n") {
			t.Fatalf("unsafe normalized folder %q from %q", got, input)
		}
		for _, part := range strings.Split(got, "/") {
			if part == ".." {
				t.Fatalf("normalized folder retained traversal segment: %q from %q", got, input)
			}
		}
	})
}
