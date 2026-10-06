// Package mailsecurity defines the canonical transport names shared by
// account persistence, validation, and protocol adapters.
package mailsecurity

import (
	"fmt"
	"strings"
)

const (
	ImplicitTLS = "implicit_tls"
	StartTLS    = "starttls"
)

// NormalizeIMAP returns the application vocabulary for an IMAP transport.
// IMAPS is accepted only as a compatibility spelling at this boundary; it is
// never returned or persisted.
func NormalizeIMAP(value string, port int) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		if port == 993 {
			return ImplicitTLS, nil
		}
		return StartTLS, nil
	}
	switch value {
	case ImplicitTLS, "imaps":
		return ImplicitTLS, nil
	case StartTLS:
		return StartTLS, nil
	default:
		return "", fmt.Errorf("unsupported IMAP security mode %q", value)
	}
}

// NormalizeSMTP returns the application vocabulary for an SMTP transport.
// SMTP has no IMAPS compatibility spelling and plaintext is deliberately not
// part of the supported account contract.
func NormalizeSMTP(value string, port int) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		if port == 465 {
			return ImplicitTLS, nil
		}
		return StartTLS, nil
	}
	if value != ImplicitTLS && value != StartTLS {
		return "", fmt.Errorf("unsupported SMTP security mode %q", value)
	}
	return value, nil
}
