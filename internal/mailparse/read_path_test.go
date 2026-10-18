package mailparse

import "testing"

func TestMaildirReadPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		read bool
		want string
	}{
		{"cur/123.msg", true, "cur/123.msg:2,S"},
		{"cur/123.msg:2,S", true, "cur/123.msg:2,S"},
		{"cur/123.msg:2,FR", true, "cur/123.msg:2,FRS"},
		{"cur/123.msg:2,S", false, "cur/123.msg:2,"},
		{"cur/123.msg:2,RS", false, "cur/123.msg:2,R"},
		{"new/456.msg", true, "new/456.msg:2,S"},
	} {
		if got := MaildirReadPath(tc.path, tc.read); got != tc.want {
			t.Fatalf("MaildirReadPath(%q, %v)=%q, want %q", tc.path, tc.read, got, tc.want)
		}
	}
}
