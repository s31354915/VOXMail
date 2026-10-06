// Package phone contains the canonical normalization used at SIP and HTTP
// ownership boundaries. It deliberately preserves the provider's telephone
// characters rather than applying locale-specific numbering assumptions.
package phone

import (
	"strings"
)

// Normalize strips SIP URI framing and parameter suffixes while preserving the
// stable telephone identity used by the whitelist and alert-number stores.
func Normalize(value string) string {
	value = strings.TrimSpace(value)
	if at := strings.IndexByte(value, '@'); at >= 0 {
		value = value[:at]
	}
	if colon := strings.LastIndexByte(value, ':'); colon >= 0 {
		value = value[colon+1:]
	}
	if semi := strings.IndexByte(value, ';'); semi >= 0 {
		value = value[:semi]
	}
	return value
}
