package scripts

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPythonRuntimeUsesFullyHashedLock(t *testing.T) {
	lock, err := os.ReadFile("../requirements/python.lock")
	if err != nil {
		t.Fatal(err)
	}
	entry := regexp.MustCompile(`^([a-zA-Z0-9_.-]+)==([a-zA-Z0-9_.+!-]+)\s+(.+)$`)
	hash := regexp.MustCompile(`^--hash=sha256:[0-9a-f]{64}$`)
	canonical := func(s string) string { return strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(s)) }
	pins := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(lock), "\\\n", " "), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		match := entry.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("unlocked or unhashed requirement %q", line)
		}
		name := canonical(match[1])
		if _, exists := pins[name]; exists {
			t.Fatalf("duplicate package %s", name)
		}
		pins[name] = match[2]
		for _, value := range strings.Fields(match[3]) {
			if !hash.MatchString(value) {
				t.Fatalf("invalid package hash for %s: %s", name, value)
			}
		}
	}
	roots, err := os.ReadFile("../requirements/python.in")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(roots), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "==")
		if len(parts) != 2 || pins[canonical(parts[0])] != parts[1] {
			t.Errorf("runtime root does not match lock: %s", line)
		}
	}
	dockerfile, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"COPY requirements/python.lock /usr/local/share/voxmail/provenance/python.lock", "--require-hashes --only-binary=:all:", "-r /usr/local/share/voxmail/provenance/python.lock", "python -m pip check", "provenance/python-lock.sha256"} {
		if !strings.Contains(string(dockerfile), required) {
			t.Errorf("runtime build missing %q", required)
		}
	}
	if strings.Count(string(dockerfile), "pip install") != 1 {
		t.Fatal("runtime has an additional unlocked pip install")
	}
	ci, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ci), "python-dependencies:") || !strings.Contains(string(ci), "run: python scripts/check-python-lock.py") {
		t.Fatal("hashed dependency acceptance is missing from CI")
	}
}
