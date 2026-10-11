package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestCIIncludesBrowserSIPAndRealProviders(t *testing.T) {
	contents, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	for _, required := range []string{
		"pull_request:",
		"VOXMAIL_E2E_SIP_PEER: '1'",
		"run: scripts/e2e.sh",
		"-tags integration -timeout 5m -v ./tests/providers",
		"install --no-install-recommends -y isync",
		"cache-dependency-path: tests/browser/package-lock.json",
		"run: npm ci",
		"npx playwright install --with-deps chromium",
		"run: scripts/browser-test.sh",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("CI acceptance boundary missing %q", required)
		}
	}
	for _, path := range []string{"browser-test.sh", "model-acceptance.sh", "require-ci.sh"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode()&0111 == 0 {
			t.Fatalf("workflow helper %s is unavailable/executable: %v", path, err)
		}
	}
}

func TestModelAcceptanceRunsPeriodicallyOnBothNativePlatforms(t *testing.T) {
	contents, err := os.ReadFile("../.github/workflows/model-acceptance.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"schedule:", "workflow_dispatch:",
		"arch: amd64", "arch: arm64", "runner: ubuntu-24.04-arm",
		"git rev-parse HEAD",
		"scripts/model-acceptance.sh voxmail-model-acceptance:${{ matrix.arch }} linux/${{ matrix.arch }}",
	} {
		if !strings.Contains(string(contents), required) {
			t.Errorf("periodic model acceptance missing %q", required)
		}
	}
	helper, err := os.ReadFile("model-acceptance.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"go run ./tests/models", "--network none", "readonly", "/usr/local/bin/voxmail-runtime-smoke", "/usr/local/bin/voxmail-real-model-smoke"} {
		if !strings.Contains(string(helper), required) {
			t.Errorf("isolated real-model acceptance missing %q", required)
		}
	}
}
