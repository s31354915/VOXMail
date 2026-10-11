package speech

// VoiceInstallError separates an actionable public reason from transport or
// process details, which can include signed download URLs or private paths.
type VoiceInstallError struct {
	Reason string
	Cause  error
}

func (e *VoiceInstallError) Error() string {
	if e.Cause == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Cause.Error()
}
func (e *VoiceInstallError) Unwrap() error { return e.Cause }
