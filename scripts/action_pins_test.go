package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Keep the guard in normal Go CI so new workflows cannot silently introduce
// mutable dependencies. Local actions inherit the checked-out source revision.
func TestWorkflowActionsUseImmutableReferences(t *testing.T) {
	files, err := filepath.Glob("../.github/workflows/*")
	if err != nil {
		t.Fatal(err)
	}
	uses := regexp.MustCompile(`^\s*(?:-\s*)?uses:\s*["']?([^\s"'#]+)`)
	commit := regexp.MustCompile(`^[0-9a-f]{40}$`)
	digest := regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	count := 0
	for _, path := range files {
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			match := uses.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			count++
			action := match[1]
			if strings.HasPrefix(action, "./") {
				continue
			}
			if strings.HasPrefix(action, "docker://") {
				if !digest.MatchString(action) {
					t.Errorf("%s:%d: container action must use an immutable digest: %s", path, lineNo+1, action)
				}
				continue
			}
			parts := strings.Split(action, "@")
			if len(parts) != 2 || !strings.Contains(parts[0], "/") || !commit.MatchString(parts[1]) {
				t.Errorf("%s:%d: remote action must use a full commit SHA: %s", path, lineNo+1, action)
			}
		}
	}
	if count == 0 {
		t.Fatal("guard did not inspect any workflow actions")
	}
}
