package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestAcceptanceWorkflowsDoNotGrantPackageWrites(t *testing.T) {
	paths, err := filepath.Glob("../.github/workflows/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if filepath.Base(path) == "publish-image.yml" {
			continue
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "packages: write") || strings.Contains(string(data), "permissions: write-all") {
			t.Errorf("non-publishing workflow %s grants package write", path)
		}
	}
}

func TestPublicationPackageWritesAreScopedToPublishers(t *testing.T) {
	contents, err := os.ReadFile("../.github/workflows/publish-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	jobsStart := strings.Index(source, "\njobs:\n")
	if jobsStart < 0 {
		t.Fatal("publication jobs missing")
	}
	if strings.Contains(source[:jobsStart], "packages: write") || strings.Contains(source[:jobsStart], "permissions: write-all") {
		t.Fatal("global publication token still grants package write")
	}
	if !strings.Contains(source[:jobsStart], "packages: read") {
		t.Fatal("private image acceptance needs package-read permission")
	}
	headers := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*):\s*$`).FindAllStringSubmatchIndex(source[jobsStart:], -1)
	if len(headers) == 0 {
		t.Fatal("permission guard did not inspect jobs")
	}
	writeJobs := map[string]bool{}
	for i, header := range headers {
		start, end := header[0], len(source[jobsStart:])
		if i+1 < len(headers) {
			end = headers[i+1][0]
		}
		name := source[jobsStart+header[2] : jobsStart+header[3]]
		job := source[jobsStart+start : jobsStart+end]
		publisher := name == "build-native" || name == "publish-manifest"
		write := strings.Contains(job, "      packages: write")
		if write != publisher {
			t.Errorf("job %s: package write=%v, publishing job=%v", name, write, publisher)
		}
		if write {
			writeJobs[name] = true
			if !strings.Contains(job, "    permissions:\n") || !strings.Contains(job, "      contents: read") {
				t.Errorf("publisher %s is missing explicit token scope", name)
			}
		}
	}
	if len(writeJobs) != 2 {
		t.Fatalf("expected two publishing jobs, found %v", writeJobs)
	}
}
