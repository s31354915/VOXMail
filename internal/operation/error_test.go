package operation

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"syscall"
	"testing"
)

func TestCategoriesUseCausesNotProviderProse(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  Category
	}{
		{context.Canceled, Canceled}, {context.DeadlineExceeded, Timeout},
		{&net.DNSError{Err: "opaque provider response"}, DNS},
		{&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, Connection},
		{x509.UnknownAuthorityError{}, TLS}, {io.EOF, Connection},
		{syscall.ENOSPC, DiskFull}, {syscall.EROFS, StoragePermissions},
		{exec.ErrNotFound, MissingDependency},
		{errors.New("authentication failed password=secret TLS timeout"), Failed},
	} {
		err := Wrap("fixture", "stage", Failed, fmt.Errorf("outer: %w", tc.cause))
		if Classify(err) != tc.want || !errors.Is(err, tc.cause) {
			t.Fatalf("category=%s want=%s error=%v", Classify(err), tc.want, err)
		}
	}
}

func TestUncertaintyOverridesTransportAndSurvivesWrapping(t *testing.T) {
	sentinel := errors.New("mutation uncertain")
	err := Wrap("smtp", "data", Uncertain, errors.Join(sentinel, context.DeadlineExceeded))
	err = Wrap("submission", "send", Protocol, err)
	if Classify(err) != Uncertain || !errors.Is(err, sentinel) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost outcome/cause: %v", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Component != "submission" {
		t.Fatalf("missing boundary: %v", err)
	}
	if Wrap("fixture", "success", Failed, nil) != nil {
		t.Fatal("wrapped success")
	}
}

func TestControllingContextOverridesTypedConnectionFailure(t *testing.T) {
	connection := Wrap("network", "dial", Connection, errors.New("opaque failure"))
	err := Wrap("imap", "connect", Protocol, errors.Join(connection, context.Canceled))
	if Classify(err) != Canceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestJoinedSecondaryUncertaintyCannotBecomeRetryable(t *testing.T) {
	connection := Wrap("network", "dial", Connection, errors.New("opaque failure"))
	uncertain := Wrap("smtp", "data", Uncertain, context.DeadlineExceeded)
	joined := errors.Join(connection, uncertain)
	if Classify(joined) != Uncertain || !errors.Is(joined, ErrOutcomeUncertain) {
		t.Fatalf("lost joined uncertainty: %v", joined)
	}
}
