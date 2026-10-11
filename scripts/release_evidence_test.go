package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeEvidenceRecordsFailuresAndSkippedModels(t *testing.T) {
	script, err := filepath.Abs("release-evidence.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"latest", "v1.2.3"} {
		t.Run(tag, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "digests"), 0700); err != nil {
				t.Fatal(err)
			}
			digest := "sha256:" + strings.Repeat("b", 64)
			if err := os.WriteFile(filepath.Join(root, "digests", "digest"), []byte(digest), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", script, "runtime", "acceptance-evidence")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "SOURCE_SHA="+strings.Repeat("a", 40), "TAG="+tag, "IMAGE=ghcr.io/example/voxmail", "ACCEPT_PLATFORM=linux/arm64", "SIP_OUTCOME=failure", "MODEL_OUTCOME=skipped")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("runtime capture: %v %s", err, out)
			}
			data, err := os.ReadFile(filepath.Join(root, "acceptance-evidence", "acceptance.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Image, Platform, SIP string
				Models               string `json:"real_models"`
				Required             bool   `json:"real_models_required"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Image != "ghcr.io/example/voxmail@"+digest || result.Platform != "linux/arm64" || result.SIP != "failure" || result.Models != "skipped" || result.Required != (tag != "latest") {
				t.Fatalf("runtime outcome misrepresented: %s", data)
			}
		})
	}
}

func TestReleaseEvidenceBindsIdentityPolicyAndAcceptance(t *testing.T) {
	for _, mode := range []string{"complete", "latest", "wrong-source", "wrong-digest", "wrong-platform", "wrong-policy", "tampered-report", "failed-sip", "missing-models", "missing-runtime", "failed-job", "no-artifacts"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			source := strings.Repeat("a", 40)
			tag := "v1.2.3"
			if mode == "latest" {
				tag = "latest"
			}
			writeJSON := func(path string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, arch := range []string{"amd64", "arm64"} {
				if mode == "no-artifacts" {
					continue
				}
				digest := "sha256:" + strings.Repeat("b", 64)
				if arch == "arm64" {
					digest = "sha256:" + strings.Repeat("c", 64)
				}
				writeJSON(filepath.Join(root, "evidence", "voxmail-digest-"+arch, "placeholder"), nil)
				if err := os.WriteFile(filepath.Join(root, "evidence", "voxmail-digest-"+arch, "digest"), []byte(digest), 0600); err != nil {
					t.Fatal(err)
				}
				report := []byte(`{"SchemaVersion":2,"Results":[]}`)
				hash := sha256.Sum256(report)
				scan := map[string]any{"source_sha": source, "image": "ghcr.io/example/voxmail@" + digest, "platform": "linux/" + arch, "scan_outcome": "success", "report_status": "available", "report_sha256": hex.EncodeToString(hash[:]), "severity": []string{"HIGH", "CRITICAL"}, "ignore_unfixed": true}
				acceptance := map[string]any{"source_sha": source, "image": scan["image"], "platform": scan["platform"], "sip": "success", "real_models": "success", "real_models_required": tag != "latest"}
				if mode == "latest" {
					acceptance["real_models"] = "skipped"
				}
				if arch == "arm64" {
					switch mode {
					case "wrong-source":
						scan["source_sha"] = strings.Repeat("d", 40)
					case "wrong-digest":
						acceptance["image"] = "ghcr.io/example/voxmail@sha256:" + strings.Repeat("e", 64)
					case "wrong-platform":
						acceptance["platform"] = "linux/amd64"
					case "wrong-policy":
						scan["ignore_unfixed"] = false
					case "tampered-report":
						report = []byte(`{"SchemaVersion":2,"Results":[{}]}`)
					case "failed-sip":
						acceptance["sip"] = "failure"
					case "missing-models":
						acceptance["real_models"] = "skipped"
					}
				}
				scanDir := filepath.Join(root, "evidence", "voxmail-scan-"+arch+"-123-1")
				writeJSON(filepath.Join(scanDir, "identity.json"), scan)
				if err := os.WriteFile(filepath.Join(scanDir, "trivy.json"), report, 0600); err != nil {
					t.Fatal(err)
				}
				if mode != "missing-runtime" || arch != "arm64" {
					writeJSON(filepath.Join(root, "evidence", "voxmail-acceptance-"+arch+"-123-1", "acceptance.json"), acceptance)
				}
			}
			script, err := filepath.Abs("release-evidence.py")
			if err != nil {
				t.Fatal(err)
			}
			run := func(action string) ([]byte, error) {
				cmd := exec.Command("python3", script, action, "evidence")
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "SOURCE_SHA="+source, "TAG="+tag, "IMAGE=ghcr.io/example/voxmail", "GITHUB_RUN_ID=123", "GITHUB_RUN_ATTEMPT=1", "VERIFY_RESULT=success", "REQUIRE_CI_RESULT=success", "BUILD_NATIVE_RESULT=success", "SCAN_NATIVE_RESULT=success", "RUNTIME_NATIVE_RESULT=success", "PUBLISH_MANIFEST_RESULT=success")
				if mode == "failed-job" || mode == "no-artifacts" {
					cmd.Env = append(cmd.Env, "RUNTIME_NATIVE_RESULT=failure", "PUBLISH_MANIFEST_RESULT=skipped")
				}
				return cmd.CombinedOutput()
			}
			out, err := run("validate")
			good := mode == "complete" || mode == "latest"
			if (err == nil) != good {
				t.Fatalf("validation mode=%s err=%v %s", mode, err, out)
			}
			if out, err := run("summary"); err != nil {
				t.Fatalf("failure summary lost: %v %s", err, out)
			}
			data, err := os.ReadFile(filepath.Join(root, "release-summary", "release.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Complete      bool     `json:"acceptance_complete"`
				Errors        []string `json:"evidence_errors"`
				Architectures []struct {
					Scan any `json:"scan"`
				} `json:"architectures"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.Complete != good || (len(result.Errors) == 0) != good {
				t.Fatalf("summary hid acceptance failure: %s", data)
			}
			if mode == "missing-runtime" && result.Architectures[1].Scan == nil {
				t.Fatal("missing runtime erased completed scan facts")
			}
		})
	}
}

func TestReleaseEvidenceWorkflowGatesAliasesAndRetainsFailureSummary(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/publish-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	publish := workflowJob(t, source, "publish-manifest", "release-summary")
	validation := strings.Index(publish, "release-evidence.py validate evidence")
	promotion := strings.Index(publish, "scripts/promote-architecture-tags.sh")
	if validation < 0 || promotion < validation {
		t.Fatal("aliases can move before exact acceptance evidence validation")
	}
	summary := source[strings.Index(source, "  release-summary:"):]
	for _, required := range []string{"always()", "needs: [verify, require-ci, build-native, scan-native, runtime-native, publish-manifest]", "release-evidence.py summary evidence", "MANIFEST_DIGEST:", "PUBLISH_MANIFEST_RESULT:", "retention-days: 90"} {
		if !strings.Contains(summary, required) {
			t.Errorf("summary missing %q", required)
		}
	}
	if strings.Contains(summary, "packages: write") || strings.Contains(summary, "crane tag") {
		t.Fatal("failure summary can mutate aliases")
	}
}
