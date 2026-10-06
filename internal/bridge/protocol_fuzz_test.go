package bridge

import (
	"bufio"
	"strings"
	"testing"
)

func FuzzDecodeNeverPanics(f *testing.F) {
	for _, seed := range []string{
		`{"version":2,"type":"dtmf","call_id":"c","digit":"1","phase":"end"}` + "\n",
		`{"version":2,"type":"event"}` + "\n",
		"\x00\n",
		"not json\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = Decode(bufio.NewReader(strings.NewReader(input)))
	})
}
