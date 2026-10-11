// Package operation describes operational failures without interpreting provider prose.
package operation

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
)

var ErrOutcomeUncertain = errors.New("operation outcome uncertain")

type Category string

const (
	Failed             Category = "operation_failed"
	Configuration      Category = "configuration"
	Timeout            Category = "timeout"
	Canceled           Category = "canceled"
	TLS                Category = "tls"
	Authentication     Category = "authentication"
	EndpointPolicy     Category = "endpoint_policy"
	DNS                Category = "dns"
	Connection         Category = "connection"
	DiskFull           Category = "disk_full"
	StoragePermissions Category = "storage_permissions"
	MissingDependency  Category = "missing_dependency"
	ProcessExit        Category = "process_exit"
	Protocol           Category = "protocol"
	Uncertain          Category = "uncertain"
	MailIndex          Category = "mail_index"
	SIPAccount         Category = "sip_account"
	Integrity          Category = "integrity"
	ResourceLimit      Category = "resource_limit"
)

// Error retains the original error for errors.Is/As. Component and Operation
// are local constants, never credentials, command arguments, or provider text.
// Error() is for restricted logs, not an API response.
type Error struct {
	Category  Category
	Component string
	Operation string
	Cause     error
}

func (e *Error) Error() string { return e.Component + " " + e.Operation + ": " + e.Cause.Error() }
func (e *Error) Unwrap() error { return e.Cause }
func (e *Error) Is(target error) bool {
	return e.Category == Uncertain && target == ErrOutcomeUncertain
}

// Wrap adds stage information while honoring a more specific typed cause.
// Uncertain is authoritative: a timeout cannot prove whether a mutation happened.
func Wrap(component, stage string, fallback Category, err error) error {
	if err == nil {
		return nil
	}
	category := Classify(err)
	if fallback == Uncertain || category == Failed {
		category = fallback
	}
	return &Error{Category: category, Component: component, Operation: stage, Cause: err}
}

// Classify uses error types and sentinels, never arbitrary error messages.
func Classify(err error) Category {
	if err == nil {
		return ""
	}
	var structured *Error
	if errors.Is(err, ErrOutcomeUncertain) {
		return Uncertain
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout
	}
	if errors.Is(err, context.Canceled) {
		return Canceled
	}
	if errors.As(err, &structured) {
		return structured.Category
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.Timeout() {
			return Timeout
		}
		return DNS
	}
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	if errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostname) {
		return TLS
	}
	if errors.Is(err, syscall.ENOSPC) {
		return DiskFull
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return StoragePermissions
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return MissingDependency
	}
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return Timeout
		}
		return Connection
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) {
		return Connection
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return ProcessExit
	}
	return Failed
}
