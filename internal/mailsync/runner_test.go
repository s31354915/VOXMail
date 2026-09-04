package mailsync

import (
	"context"
	"testing"
)

func TestRunnerRequiresChannel(t *testing.T) {
	_, err := (Runner{}).Sync(context.Background(), "/tmp/isyncrc", "")
	if err == nil {
		t.Fatal("expected missing channel error")
	}
}
