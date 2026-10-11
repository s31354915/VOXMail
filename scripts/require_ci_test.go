//go:build linux

package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireCIWaitsForExactProducerAndSource(t *testing.T) {
	sha := strings.Repeat("a", 40)
	response := fmt.Sprintf(`{"workflow_runs":[
  {"id":7,"head_sha":"%s","run_number":11,"run_attempt":1,"status":"completed","conclusion":"success"},
  {"id":8,"head_sha":"%s","run_number":12,"run_attempt":2,"status":"completed","conclusion":"success"},
  {"id":9,"head_sha":"%s","run_number":99,"run_attempt":1,"status":"completed","conclusion":"failure"}
]}`, sha, sha, strings.Repeat("b", 40))
	output, log, err := runRequireCI(t, sha, "response", response, "3")
	if err != nil {
		t.Fatalf("require-ci failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "run 12 attempt 2 succeeded") {
		t.Fatalf("require-ci did not select the latest successful run for the requested SHA: %s", output)
	}
	if !strings.Contains(log, "api --method GET repos/owner/repo/actions/workflows/ci.yml/runs") || !strings.Contains(log, "head_sha="+sha) {
		t.Fatalf("require-ci did not query the designated producer and source: %s", log)
	}
}

func TestRequireCIWaitsWhenProducerRunIsMissing(t *testing.T) {
	sha := strings.Repeat("c", 40)
	response := fmt.Sprintf(`{"workflow_runs":[{"id":10,"head_sha":"%s","run_number":2,"run_attempt":1,"status":"completed","conclusion":"success"}]}`, sha)
	output, log, err := runRequireCI(t, sha, "missing-then-success", response, "3")
	if err != nil {
		t.Fatalf("require-ci did not continue after a missing run: %v\n%s", err, output)
	}
	if !strings.Contains(output, "is missing") || !strings.Contains(output, "succeeded") || strings.Count(log, "actions/workflows/ci.yml/runs") != 2 {
		t.Fatalf("missing-to-success polling was not distinguished: output=%s log=%s", output, log)
	}
}

func TestRequireCIFailsLatestFailedRunAndReportsAPIErrors(t *testing.T) {
	sha := strings.Repeat("d", 40)
	failure := fmt.Sprintf(`{"workflow_runs":[
  {"id":1,"head_sha":"%s","run_number":1,"run_attempt":1,"status":"completed","conclusion":"success"},
  {"id":2,"head_sha":"%s","run_number":2,"run_attempt":1,"status":"completed","conclusion":"failure"}
]}`, sha, sha)
	output, _, err := runRequireCI(t, sha, "response", failure, "3")
	if err == nil || !strings.Contains(output, "failed") || !strings.Contains(output, "failure") {
		t.Fatalf("latest failed CI run was not reported: err=%v output=%s", err, output)
	}
	output, _, err = runRequireCI(t, sha, "api-error", "", "3")
	if err == nil || !strings.Contains(output, "CI status API error") || !strings.Contains(output, "rate limit") {
		t.Fatalf("GitHub API error was not distinct: err=%v output=%s", err, output)
	}
}

func TestRequireCITimesOutOnRunningProducer(t *testing.T) {
	sha := strings.Repeat("e", 40)
	response := fmt.Sprintf(`{"workflow_runs":[{"id":20,"head_sha":"%s","run_number":3,"run_attempt":1,"status":"in_progress","conclusion":null}]}`, sha)
	output, _, err := runRequireCI(t, sha, "response", response, "2")
	if err == nil || !strings.Contains(output, "timed out") || !strings.Contains(output, "status=in_progress") {
		t.Fatalf("running CI timeout was not distinct: err=%v output=%s", err, output)
	}
}

func runRequireCI(t *testing.T, sha, mode, response, attempts string) (string, string, error) {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	responsePath := filepath.Join(root, "response.json")
	if err := os.WriteFile(responsePath, []byte(response), 0600); err != nil {
		t.Fatal(err)
	}
	ghLog := filepath.Join(root, "gh.log")
	ghCalls := filepath.Join(root, "gh.calls")
	fakeGH := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$MOCK_GH_LOG"
if [ "$1" != api ] || [ "$2" != --method ] || [ "$3" != GET ]; then
  echo 'workflow-run queries must use GET, even when parameters are supplied' >&2
  exit 2
fi
calls=0
if [ -f "$MOCK_GH_CALLS" ]; then calls=$(cat "$MOCK_GH_CALLS"); fi
calls=$((calls + 1))
printf '%s' "$calls" > "$MOCK_GH_CALLS"
case "$MOCK_GH_MODE" in
  api-error) echo 'rate limit exceeded' >&2; exit 1 ;;
  missing-then-success)
    if [ "$calls" -eq 1 ]; then echo '{"workflow_runs":[]}'; else cat "$MOCK_GH_RESPONSE"; fi
    ;;
  response) cat "$MOCK_GH_RESPONSE" ;;
  *) echo 'unexpected fake gh mode' >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(fakeGH), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", "require-ci.sh", sha)
	cmd.Dir = "."
	cmd.Env = envWithOverrides(map[string]string{
		"PATH":                   binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GITHUB_REPOSITORY":      "owner/repo",
		"VOXMAIL_CI_MAX_ATTEMPTS": attempts,
		"VOXMAIL_CI_POLL_SECONDS": "0",
		"MOCK_GH_LOG":             ghLog,
		"MOCK_GH_CALLS":           ghCalls,
		"MOCK_GH_MODE":            mode,
		"MOCK_GH_RESPONSE":        responsePath,
	})
	output, err := cmd.CombinedOutput()
	log := ""
	if contents, readErr := os.ReadFile(ghLog); readErr == nil {
		log = string(contents)
	}
	return string(output), log, err
}
