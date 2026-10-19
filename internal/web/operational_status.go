package web

import (
	"context"
	"github.com/voxmail/voxmail/internal/operation"
	"github.com/voxmail/voxmail/internal/store"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OperationalFailure contains diagnostics derived from known failure signals.
// Provider/child output can contain credentials, so arbitrary output is never
// copied into the console. The durable run ID identifies the original log.
type OperationalFailure struct {
	Category   string `json:"category"`
	Reason     string `json:"reason"`
	Action     string `json:"action"`
	Diagnostic string `json:"diagnostic,omitempty"`
}

var toolExitStatus = regexp.MustCompile(`\bexit status [0-9]{1,3}\b`)
var sipFailureCode = regexp.MustCompile(`^[1-6][0-9]{2}(?:\s|$)`)

func operationalFailure(message string) *OperationalFailure {
	if strings.TrimSpace(message) == "" {
		return nil
	}
	lower := strings.ToLower(message)
	f := &OperationalFailure{Category: "operation_failed", Reason: "The operation failed.", Action: "Check the server logs for this operation, then retry."}
	contains := func(values ...string) bool {
		for _, v := range values {
			if strings.Contains(lower, v) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("interrupted by application restart"):
		f.Category, f.Reason, f.Action = "interrupted", "The previous process stopped before synchronization finished.", "Retry synchronization; downloaded mail is retained."
	case contains("deadline exceeded", "timed out", "timeout"):
		f.Category, f.Reason, f.Action = "timeout", "The operation timed out.", "Check provider connectivity and server logs, then retry."
	case contains("context canceled", "context cancelled"):
		f.Category, f.Reason, f.Action = "canceled", "The operation was canceled.", "Retry when the application is running and the operation is still needed."
	case contains("certificate", "tls handshake", "ssl", "x509"):
		f.Category, f.Reason, f.Action = "tls", "The secure connection could not be established or verified.", "Check the provider host, TLS mode, and certificate trust."
	case contains("authenticationfailed", "authentication failed", "invalid credentials", "login failed", "login rejected", "unauthorized", "authentication rejected"):
		f.Category, f.Reason, f.Action = "authentication", "The provider rejected authentication.", "Check the saved username, password or app password, and provider authentication requirements."
	case contains("outbound policy", "private address", "not a public", "endpoint rejected"):
		f.Category, f.Reason, f.Action = "endpoint_policy", "The provider endpoint was rejected by the outbound network policy.", "Use a permitted public provider endpoint and check its DNS records."
	case contains("no such host", "name resolution", "dns"):
		f.Category, f.Reason, f.Action = "dns", "The provider hostname could not be resolved.", "Check the hostname and the deployment's DNS connection."
	case contains("connection refused", "network is unreachable", "no route to host", "connection reset", "unexpected eof", "broken pipe"):
		f.Category, f.Reason, f.Action = "connection", "The provider connection failed or was interrupted.", "Check the provider address/port and network access, then retry."
	case contains("no space left", "disk full"):
		f.Category, f.Reason, f.Action = "disk_full", "The deployment ran out of disk space.", "Free space on the data volume before retrying."
	case contains("permission denied", "read-only file system"):
		f.Category, f.Reason, f.Action = "storage_permissions", "Required files could not be read or written.", "Check the data volume's ownership, permissions, and mount mode."
	case contains("executable file not found", "no such file or directory"):
		f.Category, f.Reason, f.Action = "missing_dependency", "A required executable or file is unavailable.", "Check the deployment image, configured executable paths, and installed models."
	case contains("uidvalidity", "message parse", "maildir parse", "indexing"):
		f.Category, f.Reason, f.Action = "mail_index", "Synchronization could not complete mailbox identity checks or indexing.", "Check this synchronization run's logs for the affected folder or message, then retry."
	}
	if status := toolExitStatus.FindString(lower); status != "" {
		f.Diagnostic = "Child process " + status
	}
	return f
}

// sipRegistrationFailure classifies an error the SIP runtime raised. Numeric
// response codes are only meaningful in that context: elsewhere an IMAP or
// SMTP status line such as "554 ..." would be mislabeled as a SIP registrar
// failure, so the generic classifier never applies that mapping.
func sipRegistrationFailure(message string) *OperationalFailure {
	f := operationalFailure(message)
	if f == nil {
		return nil
	}
	if code := sipFailureCode.FindString(strings.TrimSpace(message)); code != "" {
		code = strings.TrimSpace(code)
		f.Diagnostic = "SIP response " + code
		switch code {
		case "401", "403", "407":
			f.Category, f.Reason, f.Action = "authentication", "The SIP registrar rejected authentication or access.", "Check the SIP username, password, registrar, and provider account permissions."
		case "408", "504":
			f.Category, f.Reason, f.Action = "timeout", "The SIP registrar did not complete registration in time.", "Check the registrar address/port and network access."
		case "404":
			f.Category, f.Reason, f.Action = "sip_account", "The SIP registrar could not find the requested account.", "Check the SIP username and registrar."
		default:
			f.Reason, f.Action = "The SIP registrar reported a registration failure.", "Check the provider's meaning for this SIP response and the saved SIP settings."
		}
	}
	return f
}

type operationalSnapshot struct {
	CheckedAt   string                `json:"checked_at"`
	OfflineMode bool                  `json:"offline_mode"`
	Readiness   string                `json:"readiness"`
	Reason      string                `json:"reason,omitempty"`
	Runtime     RuntimeStatus         `json:"runtime"`
	SIPFailure  *OperationalFailure   `json:"sip_failure,omitempty"`
	Cleanup     *store.CleanupSummary `json:"cleanup,omitempty"`
}

func (s *Server) operationalStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status := operationalSnapshot{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), OfflineMode: s.OfflineMode, Readiness: "ready"}
	if summary, err := s.Store.CleanupSummary(ctx, u.ID, u.Role == "admin"); err == nil {
		status.Cleanup = &summary
	}
	if s.RuntimeStatus != nil {
		status.Runtime = s.RuntimeStatus(ctx)
		if status.Runtime.ErrorCategory != "" {
			status.SIPFailure = structuredFailure(status.Runtime.ErrorCategory)
			if code := status.Runtime.ResponseCode; code >= 300 && code <= 699 {
				status.SIPFailure.Diagnostic = "SIP response " + strconv.Itoa(code)
			}
		} else {
			status.SIPFailure = sipRegistrationFailure(status.Runtime.LastError)
		}
	}
	if s.shuttingDown() {
		status.Readiness, status.Reason = "not_ready", "The server is shutting down."
	} else if err := s.Store.Healthy(ctx); err != nil {
		status.Readiness, status.Reason = "not_ready", "The database is unavailable."
	} else if s.Ready != nil {
		if err := s.Ready(ctx); err != nil {
			status.Readiness, status.Reason = "not_ready", safeReadinessReason(err.Error())
		}
	}
	// A failed provider is data in this snapshot, not an HTTP transport failure.
	writeJSON(w, http.StatusOK, status)
}

func safeReadinessReason(message string) string {
	// These readiness facts are produced locally, not by a provider transcript.
	for _, reason := range []string{"call bridge not connected", "mail synchronization worker is not running", "configured Piper model is unavailable", "configured Whisper model is unavailable"} {
		if message == reason {
			return reason
		}
	}
	if strings.HasPrefix(message, "SIP process is ") || strings.HasPrefix(message, "SIP registration is ") {
		for _, state := range []string{"disabled", "starting", "running", "stopping", "stopped", "degraded", "unknown", "registering", "registered", "failed", "unregistered"} {
			if message == "SIP process is "+state || message == "SIP registration is "+state {
				return message
			}
		}
	}
	if failure := operationalFailure(message); failure != nil {
		return failure.Reason
	}
	return "The server is not ready."
}

// structuredFailure renders only local, fixed text. Persisted categories survive
// restarts without reinterpreting provider prose; unknown categories fail closed.
func structuredFailure(category string) *OperationalFailure {
	f := &OperationalFailure{Category: "operation_failed", Reason: "The operation failed.", Action: "Check the server logs for this operation, then retry."}
	switch operation.Category(category) {
	case operation.Timeout:
		f.Category, f.Reason, f.Action = category, "The operation timed out.", "Check provider connectivity and server logs, then retry."
	case operation.Canceled:
		f.Category, f.Reason, f.Action = category, "The operation was canceled.", "Retry when the application is running and the operation is still needed."
	case operation.TLS:
		f.Category, f.Reason, f.Action = category, "The secure connection could not be established or verified.", "Check the provider host, TLS mode, and certificate trust."
	case operation.Authentication:
		f.Category, f.Reason, f.Action = category, "The provider rejected authentication.", "Check the saved username, password or app password, and provider authentication requirements."
	case operation.EndpointPolicy:
		f.Category, f.Reason, f.Action = category, "The provider endpoint was rejected by the outbound network policy.", "Use a permitted public provider endpoint and check its DNS records."
	case operation.DNS:
		f.Category, f.Reason, f.Action = category, "The provider hostname could not be resolved.", "Check the hostname and the deployment's DNS connection."
	case operation.Connection:
		f.Category, f.Reason, f.Action = category, "The provider connection failed or was interrupted.", "Check the provider address/port and network access, then retry."
	case operation.Configuration:
		f.Category, f.Reason, f.Action = category, "Required settings are incomplete or unsupported.", "Check the saved provider and deployment settings."
	case operation.DiskFull:
		f.Category, f.Reason, f.Action = category, "The deployment ran out of disk space.", "Free space on the data volume before retrying."
	case operation.StoragePermissions:
		f.Category, f.Reason, f.Action = category, "Required files could not be read or written.", "Check the data volume's ownership, permissions, and mount mode."
	case operation.MissingDependency:
		f.Category, f.Reason, f.Action = category, "A required executable or file is unavailable.", "Check the deployment image, configured executable paths, and installed models."
	case operation.MailIndex:
		f.Category, f.Reason, f.Action = category, "Synchronization could not complete mailbox identity checks or indexing.", "Check this synchronization run's logs for the affected folder or message, then retry."
	case operation.SIPAccount:
		f.Category, f.Reason, f.Action = category, "The SIP registrar could not find the requested account.", "Check the SIP username and registrar."
	case operation.Integrity:
		f.Category, f.Reason, f.Action = category, "An artifact failed integrity verification.", "Check the configured artifact source and expected checksum."
	case operation.ResourceLimit:
		f.Category, f.Reason, f.Action = category, "The operation exceeded its resource limit.", "Check the server logs for the affected input or output."
	case operation.ProcessExit:
		f.Category, f.Reason = category, "A required child process failed."
	case operation.Protocol:
		f.Category, f.Reason = category, "The provider rejected the operation or returned an invalid response."
	case operation.Uncertain:
		f.Category, f.Reason, f.Action = category, "The provider operation's outcome is uncertain.", "Check the remote state before retrying to avoid repeating the operation."
	case "interrupted":
		f.Category, f.Reason, f.Action = category, "The previous process stopped before synchronization finished.", "Retry synchronization; downloaded mail is retained."
	}
	return f
}

func failureForError(err error) *OperationalFailure {
	if err == nil {
		return nil
	}
	return structuredFailure(string(operation.Classify(err)))
}

func writeOperationalError(w http.ResponseWriter, status int, err error) {
	failure := failureForError(err)
	writeJSON(w, status, apiErrorResponse{Error: failure.Reason + " " + failure.Action, Code: apiErrorCode(status), RequestID: w.Header().Get("X-Request-ID"), Failure: failure})
}
