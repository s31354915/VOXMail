package mailconfig

import (
	"encoding/base64"
	"strings"
)

// imapUTF7 encodes a mailbox name in the modified UTF-7 form RFC 3501
// requires for IMAP mailbox names. A literal ampersand is the UTF-7 shift
// character, so it must become "&-"; runs of non-ASCII characters are
// base64-encoded in the modified alphabet ("," replaces "/", no padding).
// mbsync passes the configured mailbox names to the server verbatim, so a
// folder name containing an unencoded "&" is rejected with
// "BAD Could not parse command" before the channel ever syncs.
func imapUTF7(name string) string {
	if !strings.ContainsFunc(name, func(r rune) bool {
		return r == '&' || r < 0x20 || r > 0x7e
	}) {
		return name
	}
	var b strings.Builder
	runes := []rune(name)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '&' {
			b.WriteString("&-")
			continue
		}
		if r >= 0x20 && r <= 0x7e {
			b.WriteRune(r)
			continue
		}
		j := i
		for j < len(runes) && runes[j] != '&' && (runes[j] < 0x20 || runes[j] > 0x7e) {
			j++
		}
		var units []uint16
		for _, rr := range runes[i:j] {
			if rr >= 0x10000 {
				rr -= 0x10000
				units = append(units, uint16(0xd800+(rr>>10)), uint16(0xdc00+(rr&0x3ff)))
			} else {
				units = append(units, uint16(rr))
			}
		}
		buf := make([]byte, 0, len(units)*2)
		for _, u := range units {
			buf = append(buf, byte(u>>8), byte(u))
		}
		enc := base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString(buf)
		enc = strings.ReplaceAll(enc, "/", ",")
		b.WriteByte('&')
		b.WriteString(enc)
		b.WriteByte('-')
		i = j - 1
	}
	return b.String()
}
