package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestDependencyUpdatesCoverManifestsAndRequireReview(t *testing.T) {
	config, err := os.ReadFile("../.github/dependabot.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"version: 2", "package-ecosystem: docker", "package-ecosystem: github-actions", "package-ecosystem: gomod", "package-ecosystem: npm", "package-ecosystem: pip", "/internal/web/console", "/tests/browser", "directory: /requirements"} {
		if !strings.Contains(string(config), value) {
			t.Errorf("dependency update configuration missing %q", value)
		}
	}
	if strings.Count(string(config), "interval: weekly") != 5 || strings.Count(string(config), "open-pull-requests-limit: 5") != 5 {
		t.Fatal("every ecosystem must have a bounded weekly proposal schedule")
	}
	manifest, err := os.ReadFile("../requirements/requirements.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "-r python.in") {
		t.Fatal("Dependabot cannot discover runtime roots")
	}
	workflow, err := os.ReadFile("../.github/workflows/python-lock-updates.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(workflow)
	proposeStart := strings.Index(source, "  propose:\n")
	if proposeStart < 0 {
		t.Fatal("review proposal job missing")
	}
	generate, propose := source[:proposeStart], source[proposeStart:]
	for _, value := range []string{"workflow_dispatch:", "schedule:", "timeout-minutes: 20", "python scripts/lock-python.py", "python scripts/check-python-lock.py", "persist-credentials: false", "if-no-files-found: error", "path: requirements/python.lock"} {
		if !strings.Contains(generate, value) {
			t.Errorf("read-only generation missing %q", value)
		}
	}
	if strings.Contains(generate, "contents: write") || strings.Contains(generate, "pull-requests: write") {
		t.Fatal("resolution/verification may not receive repository write credentials")
	}
	for _, value := range []string{"needs: generate", "contents: write", "pull-requests: write", "ref: ${{ needs.generate.outputs.source_sha }}", "persist-credentials: false", "add-paths: requirements/python.lock", "draft: always-true", "branch: automation/python-runtime-lock"} {
		if !strings.Contains(propose, value) {
			t.Errorf("restricted draft proposal missing %q", value)
		}
	}
	if strings.Contains(propose, "python scripts/") || strings.Contains(source, "packages: write") || strings.Contains(source, "(publish)") || strings.Contains(source, "--auto") {
		t.Fatal("proposal job must not execute new dependencies, publish or auto-merge")
	}
	ci, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ci), "types: [opened, synchronize, reopened, ready_for_review]") {
		t.Fatal("maintainer review transition must trigger PR CI")
	}
}
