package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanEvidencePreservesFailureAndDetectsMissingReports(t *testing.T) {
	for _, tc := range []struct {
		name, report, outcome, status string
		fail                          bool
	}{
		{"success", `{"SchemaVersion":2,"Results":[]}`, "success", "available", false},
		{"blocked", `{"SchemaVersion":2,"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-fixture","Severity":"CRITICAL"}]}]}`, "failure", "available", false},
		{"unavailable", "", "failure", "missing", true},
		{"malformed", `not json`, "success", "invalid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			run := func(action string) ([]byte, error) {
				cmd := exec.Command("python3", "scan-evidence.py", action, directory)
				cmd.Env = append(os.Environ(), "SOURCE_SHA="+strings.Repeat("a", 40), "SCAN_IMAGE=ghcr.io/example/voxmail@sha256:"+strings.Repeat("b", 64), "SCAN_PLATFORM=linux/arm64", "SCAN_OUTCOME="+tc.outcome)
				return cmd.CombinedOutput()
			}
			if out, err := run("prepare"); err != nil {
				t.Fatalf("prepare: %v %s", err, out)
			}
			if tc.report != "" {
				if err := os.WriteFile(filepath.Join(directory, "trivy.json"), []byte(tc.report), 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := run("finish")
			if (err != nil) != tc.fail {
				t.Fatalf("finish: %v %s", err, out)
			}
			data, err := os.ReadFile(filepath.Join(directory, "identity.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest["scan_outcome"] != tc.outcome || manifest["report_status"] != tc.status || manifest["platform"] != "linux/arm64" {
				t.Fatalf("lost outcome/identity: %s", data)
			}
			if tc.status == "available" {
				if digest, ok := manifest["report_sha256"].(string); !ok || len(digest) != 64 {
					t.Fatal("missing report integrity")
				}
			}
		})
	}
}

func TestScanReportUploadRunsAfterFailedScanner(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/publish-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	scan := workflowJob(t, string(data), "scan-native", "runtime-native")
	for _, required := range []string{"format: json", "output: scan-evidence/trivy.json", "id: scan", "SCAN_OUTCOME: ${{ steps.scan.outcome }}", "run: python3 scripts/scan-evidence.py finish scan-evidence", "name: voxmail-scan-${{ matrix.arch }}-${{ github.run_id }}-${{ github.run_attempt }}", "retention-days: 30", "if-no-files-found: error"} {
		if !strings.Contains(scan, required) {
			t.Errorf("scan retention missing %q", required)
		}
	}
	if strings.Count(scan, "if: always()") != 2 || strings.Contains(scan, "continue-on-error") {
		t.Fatal("evidence retention must run on failure without converting scan failure to success")
	}
}
