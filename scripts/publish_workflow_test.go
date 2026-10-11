package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishWorkflowDoesNotMoveReleaseTagsBeforeScans(t *testing.T) {
	workflow, err := os.ReadFile("../.github/workflows/publish-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(workflow)
	concurrencyStart := strings.Index(source, "concurrency:\n")
	permissionsStart := strings.Index(source, "permissions:\n")
	if concurrencyStart < 0 || permissionsStart <= concurrencyStart {
		t.Fatal("workflow concurrency block was not found")
	}
	concurrency := source[concurrencyStart:permissionsStart]
	if !strings.Contains(concurrency, "queue: max") || strings.Contains(concurrency, "cancel-in-progress") {
		t.Fatalf("publication queue must retain pending runs: %q", concurrency)
	}
	build := workflowJob(t, source, "build-native", "scan-native")
	requireCI := workflowJob(t, source, "require-ci", "build-native")
	scan := workflowJob(t, source, "scan-native", "runtime-native")
	runtime := workflowJob(t, source, "runtime-native", "publish-manifest")
	publish := workflowJob(t, source, "publish-manifest", "release-summary")
	permissions := source[permissionsStart:strings.Index(source, "\nenv:\n")]
	if !strings.Contains(permissions, "actions: read") {
		t.Fatal("workflow token cannot read the designated CI producer runs")
	}
	for _, required := range []string{
		"uses: actions/checkout@",
		"ref: ${{ github.workflow_sha }}",
		"timeout-minutes: 70",
		"GH_TOKEN: ${{ github.token }}",
		"SOURCE_SHA: ${{ needs.verify.outputs.source_sha }}",
		"run: scripts/require-ci.sh \"$SOURCE_SHA\"",
	} {
		if !strings.Contains(requireCI, required) {
			t.Errorf("CI gate is missing %q", required)
		}
	}
	requireCIScript, err := os.ReadFile("require-ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"actions/workflows/ci.yml/runs",
		"gh api --method GET",
		"head_sha=$source_sha",
		"queued|in_progress|waiting|requested|pending",
		"timed out waiting",
		"CI status API error",
	} {
		if !strings.Contains(string(requireCIScript), required) {
			t.Errorf("CI gate script is missing %q", required)
		}
	}
	for _, required := range []string{
		"staging-${{ needs.verify.outputs.source_sha }}-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.arch }}",
		"id: build",
		"BUILD_DIGEST: ${{ steps.build.outputs.digest }}",
		"name: voxmail-digest-${{ matrix.arch }}",
	} {
		if !strings.Contains(build, required) {
			t.Errorf("native build is missing %q", required)
		}
	}
	if strings.Contains(build, "${{ env.IMAGE }}:${{ env.TAG }}-${{ matrix.arch }}") {
		t.Fatal("native build still moves a release architecture tag before scanning")
	}
	for _, required := range []string{
		"needs: [verify, build-native]",
		"name: voxmail-digest-${{ matrix.arch }}",
		"ref=${IMAGE}@${digest}",
		"image-ref: ${{ steps.image.outputs.ref }}",
		"exit-code: '1'",
	} {
		if !strings.Contains(scan, required) {
			t.Errorf("native scan is missing %q", required)
		}
	}
	for _, required := range []string{
		"needs: [verify, build-native, scan-native]",
		"ref: ${{ github.workflow_sha }}",
		"name: voxmail-digest-${{ matrix.arch }}",
		"docker run --rm --network host",
		"--platform \"linux/${{ matrix.arch }}\"",
		`"${IMAGE}@${digest}"`,
		"if: env.TAG != 'latest'",
		`scripts/model-acceptance.sh "${IMAGE}@${digest}" "linux/${{ matrix.arch }}"`,
	} {
		if !strings.Contains(runtime, required) {
			t.Errorf("native runtime test is missing %q", required)
		}
	}
	for _, required := range []string{
		"needs: [verify, require-ci, build-native, scan-native, runtime-native]",
		"SCAN_RESULT: ${{ needs.scan-native.result }}",
		"scripts/promote-architecture-tags.sh \"$SCAN_RESULT\" \"$IMAGE\" \"$TAG\" digests",
		"docker buildx imagetools create",
		`--tag "${IMAGE}:${staging_tag}"`,
		`"${IMAGE}@${amd64_digest}"`,
		`"${IMAGE}@${arm64_digest}"`,
	} {
		if !strings.Contains(publish, required) {
			t.Errorf("publish job is missing %q", required)
		}
	}
	if !strings.Contains(publish, `crane tag "${IMAGE}@${release_digest}" latest`) {
		t.Fatal("latest promotion is not a remote tag of the scanned release digest")
	}
	if strings.Contains(publish, "always()") {
		t.Fatal("publish job can run after a failed scan or runtime acceptance job")
	}
}

func TestArchitectureAliasPromotionRefusesFailedScanWithoutCallingCrane(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	craneCalls := filepath.Join(tmp, "crane-calls")
	fakeCrane := filepath.Join(bin, "crane")
	if err := os.WriteFile(fakeCrane, []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> \"$CRANE_CALL_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "promote-architecture-tags.sh", "failure", "ghcr.io/example/voxmail", "v1.2.3", filepath.Join(tmp, "digests"))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CRANE_CALL_LOG="+craneCalls)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("failed scan was accepted; output: %s", output)
	}
	if !strings.Contains(string(output), "refusing architecture alias promotion: scan result is failure") {
		t.Fatalf("unexpected guard result: %s", output)
	}
	if calls, err := os.ReadFile(craneCalls); err == nil && len(calls) != 0 {
		t.Fatalf("failed scan moved or inspected an alias: %s", calls)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestArchitectureAliasPromotionTagsBothScannedDigests(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	digestDir := filepath.Join(tmp, "digests")
	for _, dir := range []string{bin, filepath.Join(digestDir, "amd64"), filepath.Join(digestDir, "arm64")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	digests := map[string]string{"amd64": "sha256:" + strings.Repeat("a", 64), "arm64": "sha256:" + strings.Repeat("b", 64)}
	for arch, digest := range digests {
		if err := os.WriteFile(filepath.Join(digestDir, arch, "digest"), []byte(digest+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	craneCalls := filepath.Join(tmp, "crane-calls")
	fakeCrane := filepath.Join(bin, "crane")
	fakeCraneSource := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CRANE_CALL_LOG"
if [[ "$1" == digest ]]; then
  case "$2" in
    *-amd64) cat "$DIGEST_DIR/amd64/digest" ;;
    *-arm64) cat "$DIGEST_DIR/arm64/digest" ;;
    *) printf 'sha256:%064d\n' 0 ;;
  esac
fi
`
	if err := os.WriteFile(fakeCrane, []byte(fakeCraneSource), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "promote-architecture-tags.sh", "success", "ghcr.io/example/voxmail", "v1.2.3", digestDir, "sha256:"+strings.Repeat("0", 64))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CRANE_CALL_LOG="+craneCalls, "DIGEST_DIR="+digestDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("promotion failed: %v: %s", err, output)
	}
	calls, err := os.ReadFile(craneCalls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tag ghcr.io/example/voxmail@" + digests["amd64"] + " v1.2.3-amd64",
		"digest ghcr.io/example/voxmail:v1.2.3-amd64",
		"tag ghcr.io/example/voxmail@" + digests["arm64"] + " v1.2.3-arm64",
		"digest ghcr.io/example/voxmail:v1.2.3-arm64",
	} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("crane calls missing %q:\n%s", want, calls)
		}
	}
}

func TestFailedArchitectureScanBlocksEveryReleaseAliasMove(t *testing.T) {
	workflow, err := os.ReadFile("../.github/workflows/publish-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(workflow)
	build := workflowJob(t, source, "build-native", "scan-native")
	scan := workflowJob(t, source, "scan-native", "runtime-native")
	publishStart := strings.Index(source, "  publish-manifest:\n")
	if publishStart < 0 {
		t.Fatal("publish-manifest job was not found")
	}
	publish := workflowJob(t, source, "publish-manifest", "release-summary")
	publishHeader, _, ok := strings.Cut(publish, "    steps:\n")
	if !ok {
		t.Fatal("publish-manifest steps were not found")
	}

	for _, required := range []string{
		"staging-${{ needs.verify.outputs.source_sha }}-${{ github.run_id }}-${{ github.run_attempt }}-${{ matrix.arch }}",
	} {
		if !strings.Contains(build, required) {
			t.Errorf("architecture build is missing unique staging tag %q", required)
		}
	}
	if strings.Contains(build, "${{ env.TAG }}-${{ matrix.arch }}") {
		t.Fatal("architecture build moves a public release alias before scanning")
	}
	for _, required := range []string{
		"needs: [verify, build-native]",
		"arch: [amd64, arm64]",
		"exit-code: '1'",
	} {
		if !strings.Contains(scan, required) {
			t.Errorf("architecture scan is missing failure-gate requirement %q", required)
		}
	}
	for _, required := range []string{
		"scan-native",
		"build-native",
		"runtime-native",
	} {
		if !strings.Contains(publishHeader, required) {
			t.Errorf("publish job can run without successful dependency %q", required)
		}
	}
	if strings.Contains(publishHeader, "if:") || strings.Contains(publish, "always()") {
		t.Fatal("publish job overrides the default successful-dependencies gate")
	}
	for _, aliasMove := range []string{
		"crane tag",
		"docker buildx imagetools create",
	} {
		if strings.Contains(source[:publishStart], aliasMove) {
			t.Errorf("release alias operation %q runs before the gated publish job", aliasMove)
		}
	}
}

func TestRetaggingStagingAliasCannotChangeScannedRuntimeOrPublishedDigest(t *testing.T) {
	workflow, err := os.ReadFile("../.github/workflows/publish-image.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(workflow)
	build := workflowJob(t, source, "build-native", "scan-native")
	scan := workflowJob(t, source, "scan-native", "runtime-native")
	runtime := workflowJob(t, source, "runtime-native", "publish-manifest")
	publish := workflowJob(t, source, "publish-manifest", "release-summary")

	for _, required := range []string{
		"BUILD_DIGEST: ${{ steps.build.outputs.digest }}",
		"printf '%s\\n' \"$BUILD_DIGEST\" > digest",
		"name: voxmail-digest-${{ matrix.arch }}",
	} {
		if !strings.Contains(build, required) {
			t.Errorf("build does not preserve its output digest: missing %q", required)
		}
	}
	for _, required := range []string{
		"name: voxmail-digest-${{ matrix.arch }}",
		"ref=${IMAGE}@${digest}",
		"image-ref: ${{ steps.image.outputs.ref }}",
	} {
		if !strings.Contains(scan, required) {
			t.Errorf("scan is not tied to the build digest artifact: missing %q", required)
		}
	}
	for _, required := range []string{
		"name: voxmail-digest-${{ matrix.arch }}",
		`"${IMAGE}@${digest}"`,
	} {
		if !strings.Contains(runtime, required) {
			t.Errorf("runtime check is not tied to the build digest artifact: missing %q", required)
		}
	}
	for _, required := range []string{
		"name: voxmail-digest-amd64",
		"name: voxmail-digest-arm64",
		`"${IMAGE}@${amd64_digest}"`,
		`"${IMAGE}@${arm64_digest}"`,
	} {
		if !strings.Contains(publish, required) {
			t.Errorf("manifest is not built from both captured digests: missing %q", required)
		}
	}
	for stage, job := range map[string]string{"scan": scan, "runtime": runtime, "publish": publish} {
		if strings.Contains(job, "${IMAGE}:staging-") || strings.Contains(job, "${IMAGE}:${TAG}-${{ matrix.arch }}") {
			t.Errorf("%s resolves a mutable staging or architecture alias instead of the captured digest", stage)
		}
	}
}

func workflowJob(t *testing.T, source, name, next string) string {
	t.Helper()
	start := strings.Index(source, "  "+name+":\n")
	if start < 0 {
		t.Fatalf("workflow job %q not found", name)
	}
	end := strings.Index(source[start+1:], "  "+next+":\n")
	if end < 0 {
		t.Fatalf("workflow job %q not followed by %q", name, next)
	}
	return source[start : start+1+end]
}
