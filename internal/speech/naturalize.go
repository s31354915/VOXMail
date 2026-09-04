package speech

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

var (
	garbageRE  = regexp.MustCompile(`(?is)<(?:script|style|head)[^>]*>.*?</(?:script|style|head)>`)
	anchorRE   = regexp.MustCompile(`(?is)<a[^>]*href=["'][^"']+["'][^>]*>(.*?)</a>`)
	tagRE      = regexp.MustCompile(`(?is)<[^>]+>`)
	spaceRE    = regexp.MustCompile(`[ \t\r\n]+`)
	urlRE      = regexp.MustCompile(`(?i)\bhttps?://[^\s<>]+`)
	emailRE    = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	percentRE  = regexp.MustCompile(`\b(\d+(?:\.\d+)?)%`)
	currencyRE = regexp.MustCompile(`([$€£])\s?(\d+(?:[.,]\d{2})?)`)
	phoneRE    = regexp.MustCompile(`(?:\+?\d[\d .()\-]{6,}\d)`)
)

// EmailToSpeech converts common technical tokens to words without exposing
// raw tracking URLs. It deliberately remains deterministic and local.
func EmailToSpeech(input string) string {
	input = html.UnescapeString(input)
	input = garbageRE.ReplaceAllString(input, " ")
	input = anchorRE.ReplaceAllString(input, "link $1")
	input = tagRE.ReplaceAllString(input, " ")
	input = urlRE.ReplaceAllStringFunc(input, func(raw string) string {
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			return "link"
		}
		return "link"
	})
	input = emailRE.ReplaceAllStringFunc(input, spellEmail)
	input = percentRE.ReplaceAllString(input, `$1 percent`)
	input = currencyRE.ReplaceAllStringFunc(input, func(raw string) string {
		return strings.ReplaceAll(raw, ".", " point ")
	})
	input = phoneRE.ReplaceAllStringFunc(input, groupDigits)
	input = spaceRE.ReplaceAllString(input, " ")
	return strings.TrimSpace(input)
}

func spellEmail(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "@", " at ")
	value = strings.ReplaceAll(value, ".", " dot ")
	value = strings.ReplaceAll(value, "-", " dash ")
	value = strings.ReplaceAll(value, "_", " underscore ")
	return value
}

func groupDigits(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
			b.WriteByte(' ')
		} else if r == '+' {
			b.WriteString(" plus ")
		}
	}
	return b.String()
}
