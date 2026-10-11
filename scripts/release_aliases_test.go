package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseAliasesRejectConflictsBeforeAnyPublicWrite(t *testing.T) {
	for _, mode := range []string{"new", "identical", "partial-identical", "conflict-amd64", "conflict-arm64", "conflict-manifest", "list-failure", "digest-failure", "latest", "bad-arm64"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			digests := filepath.Join(root, "digests")
			for _, arch := range []string{"amd64", "arm64"} {
				if err := os.MkdirAll(filepath.Join(digests, arch), 0700); err != nil {
					t.Fatal(err)
				}
				value := "sha256:" + strings.Repeat("a", 64)
				if arch == "arm64" {
					value = "sha256:" + strings.Repeat("b", 64)
					if mode == "bad-arm64" {
						value = "invalid"
					}
				}
				if err := os.WriteFile(filepath.Join(digests, arch, "digest"), []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			fake := `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "$CALLS"
if [[ $1 == ls ]]; then
  [[ $MODE != list-failure ]] || exit 2
  case $MODE in
    new) ;;
    partial-identical) echo v1-amd64 ;;
    *) printf '%s\n' v1-amd64 v1-arm64 v1 ;;
  esac
elif [[ $1 == tag ]]; then
  echo "$*" >> "$WRITES"
elif [[ $1 == digest ]]; then
  [[ $MODE != digest-failure ]] || exit 3
  case $2 in
    *-amd64) letter=a; conflict=conflict-amd64 ;;
    *-arm64) letter=b; conflict=conflict-arm64 ;;
    *) letter=c; conflict=conflict-manifest ;;
  esac
  if [[ $MODE == "$conflict" ]]; then letter=d; fi
  printf 'sha256:'
  for ((i=0;i<64;i++)); do printf '%s' "$letter"; done
  printf '\n'
else
  exit 4
fi
`
			if err := os.WriteFile(filepath.Join(root, "crane"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			tag := "v1"
			if mode == "latest" {
				tag = "latest"
			}
			cmd := exec.Command("bash", "promote-architecture-tags.sh", "success", "ghcr.io/example/voxmail", tag, digests, "sha256:"+strings.Repeat("c", 64))
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "MODE="+mode, "CALLS="+filepath.Join(root, "calls"), "WRITES="+filepath.Join(root, "writes"))
			out, err := cmd.CombinedOutput()
			good := mode == "new" || mode == "identical" || mode == "partial-identical" || mode == "latest"
			if (err == nil) != good {
				t.Fatalf("mode=%s err=%v output=%s", mode, err, out)
			}
			writes, readErr := os.ReadFile(filepath.Join(root, "writes"))
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			if !good && len(writes) != 0 {
				t.Fatalf("conflict/error wrote public aliases: %s", writes)
			}
			if good && strings.Count(string(writes), "\n") != 3 {
				t.Fatalf("did not publish all three exact aliases: %s", writes)
			}
			if mode == "latest" {
				calls, err := os.ReadFile(filepath.Join(root, "calls"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(calls), "ls ") {
					t.Fatal("mutable latest was subject to version existence guard")
				}
			}
		})
	}
}

func TestReleaseAliasWorkflowStagesManifestBeforePolicyAndPublicWrites(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/publish-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	publish := workflowJob(t, string(data), "publish-manifest", "release-summary")
	stage := strings.Index(publish, `--tag "${IMAGE}:${staging_tag}"`)
	policy := strings.Index(publish, `scripts/promote-architecture-tags.sh "$SCAN_RESULT" "$IMAGE" "$TAG" digests "$MANIFEST_DIGEST"`)
	if stage < 0 || policy <= stage {
		t.Fatal("manifest must be staged before all-alias policy enforcement")
	}
	if strings.Contains(publish, `--tag "${IMAGE}:${TAG}"`) {
		t.Fatal("imagetools bypasses immutable release alias policy")
	}
}
