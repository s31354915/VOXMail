package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestCIProducerVerifiesExactSourceForEveryCheckout(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	if !strings.Contains(source, "VOXMAIL_CI_SOURCE_SHA: ${{ github.event.pull_request.head.sha || github.sha }}") {
		t.Fatal("PR producer must select the head SHA that the release gate queries")
	}
	sections := strings.Split(source, "      - uses: actions/checkout@")
	if len(sections) < 2 {
		t.Fatal("CI has no source checkout")
	}
	for i, section := range sections[1:] {
		end := strings.Index(section, "      - name: Verify exact checked-out source\n")
		if end < 0 {
			t.Fatalf("checkout %d has no executable source identity assertion", i+1)
		}
		checkout := section[:end]
		if !strings.Contains(checkout, "ref: ${{ env.VOXMAIL_CI_SOURCE_SHA }}") || !strings.Contains(checkout, "persist-credentials: false") {
			t.Fatalf("checkout %d can test a different SHA or persist credentials", i+1)
		}
		assertion := section[end:]
		next := strings.Index(assertion[len("      - name:"):], "\n      - ")
		if next >= 0 {
			assertion = assertion[:len("      - name:")+next]
		}
		if !strings.Contains(assertion, `run: test "$(git rev-parse HEAD)" = "$VOXMAIL_CI_SOURCE_SHA"`) {
			t.Fatalf("checkout %d never compares the real checkout with the selected source", i+1)
		}
	}
}
