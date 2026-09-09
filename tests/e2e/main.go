// vuxmail.e2e is a "killer" end-to-end harness that runs against a live
// VOXMail container over HTTP. It is intentionally aggressive: every write
// path is poked with malformed input, cross-user isolation is verified, the
// sign-in brute-force guard is exercised over one TCP connection, and phase 2
// (run with -after-restart) proves that state survives a container restart.
//
// The harness only imports the standard library so it builds in seconds.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var failures int

func check(cond bool, format string, args ...any) {
	if cond {
		fmt.Printf("  ok   %s\n", fmt.Sprintf(format, args...))
		return
	}
	failures++
	fmt.Printf("  FAIL %s\n", fmt.Sprintf(format, args...))
}

func fatalf(format string, args ...any) {
	failures++
	fmt.Printf("  FAIL %s\n", fmt.Sprintf(format, args...))
}

// sess is one authenticated HTTP session with its own cookie jar and CSRF.
type sess struct {
	base string
	user string
	c    *http.Client
	csrf string
}

func newSess(base string) *sess {
	jar, _ := cookiejar.New(nil)
	return &sess{base: base, c: &http.Client{Jar: jar}}
}

func (s *sess) do(method, path string, body any, csrfOK bool) (*http.Response, []byte) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			fatalf("marshal %s %s: %v", method, path, err)
			return nil, nil
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, s.base+path, reader)
	if err != nil {
		fatalf("new request %s %s: %v", method, path, err)
		return nil, nil
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrfOK && s.csrf != "" && method != "GET" && method != "HEAD" {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	resp, err := s.c.Do(req)
	if err != nil {
		fatalf("request %s %s: %v", method, path, err)
		return nil, nil
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		fatalf("read %s %s: %v", method, path, err)
	}
	return resp, data
}

// must performs a request and asserts the exact status code.
func (s *sess) must(method, path string, body any, want int) []byte {
	resp, data := s.do(method, path, body, true)
	if resp == nil {
		return nil
	}
	check(resp.StatusCode == want, "%s %s -> %d (want %d)", method, path, resp.StatusCode, want)
	if resp.StatusCode != want && len(data) > 0 {
		fmt.Printf("       body: %s\n", strings.TrimSpace(string(data)))
	}
	return data
}

// withoutCSRF performs the same request but deliberately omits the CSRF header.
func (s *sess) withoutCSRF(method, path string, body any, want int) {
	req, _ := http.NewRequest(method, s.base+path, bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.c.Do(req)
	if err != nil {
		fatalf("no-csrf %s %s: %v", method, path, err)
		return
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	check(resp.StatusCode == want, "no-CSRF %s %s -> %d (want %d)", method, path, resp.StatusCode, want)
	if resp.StatusCode != want && len(data) > 0 {
		fmt.Printf("       body: %s\n", strings.TrimSpace(string(data)))
	}
}

func (s *sess) login(username, password, totp string) bool {
	resp, data := s.do("POST", "/api/v1/login", map[string]any{"username": username, "password": password, "totp": totp}, false)
	if resp == nil {
		return false
	}
	check(resp.StatusCode == 200, "POST /api/v1/login -> %d (want 200)", resp.StatusCode)
	if resp.StatusCode != 200 {
		return false
	}
	s.csrf = resp.Header.Get("X-CSRF-Token")
	check(s.csrf != "", "session carries a CSRF token after sign-in")
	var me map[string]any
	_ = json.Unmarshal(data, &me)
	s.user, _ = me["id"].(string)
	return true
}

func (s *sess) withCSRF(h string) { s.csrf = h }

func bodyContains(data []byte, sub string) bool {
	return data != nil && strings.Contains(string(data), sub)
}

// bruteforce exercises the login rate-limit gate over a single TCP connection
// so every attempt shares the same RemoteAddr and cannot spread across ports.
// expected holds the status code for each attempt in order.
func bruteforce(base, username, password string, expected []int) {
	u, _ := url.Parse(base)
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		fatalf("dial for bruteforce: %v", err)
		return
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for i, want := range expected {
		body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
		fmt.Fprintf(conn, "POST /api/v1/login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\n%s", u.Host, len(body), body)
		status, err := rawStatus(reader)
		if err != nil {
			fatalf("bruteforce attempt %d: %v", i+1, err)
			return
		}
		check(status == want, "login attempt %d -> %d (want %d)", i+1, status, want)
	}
}

func rawStatus(reader *bufio.Reader) (int, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return 0, err
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 {
		return 0, errors.New("malformed status line")
	}
	return strconv.Atoi(parts[1])
}

func waitReady(base string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", base+"/readyz", nil)
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

const (
	adminUser = "administrator"
	adminPass = "s3cret-voxmail-e2e!"
	adminPIN  = "1234"
	bobUser   = "bob"
	bobPass   = "bob-password-long!"
	bobPIN    = "5678"
	phone     = "+15551212"
)

func main() {
	base := os.Getenv("VOXMAIL_URL")
	if base == "" {
		base = "http://127.0.0.1:18080"
	}
	if len(os.Args) > 1 && os.Args[1] == "-after-restart" {
		afterRestart(base)
	} else {
		phase1(base)
	}
	if failures > 0 {
		fmt.Printf("\n%d E2E check(s) FAILED\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nall E2E checks passed")
}

func phase1(base string) {
	check(waitReady(base, 90*time.Second), "container is ready (/readyz)")
	admin := newSess(base)

	fmt.Println("liveness and first-run state")
	resp, data := admin.do("GET", "/api/v1", nil, false)
	check(resp.StatusCode == 200 && bodyContains(data, `"setup_available":true`), "GET /api/v1 reports first-run setup available")
	admin.must("GET", "/healthz", nil, 200)
	admin.must("GET", "/api/v1/me", nil, 401)

	fmt.Println("first-run setup")
	admin.must("POST", "/api/v1/setup", map[string]any{"username": adminUser, "password": "short", "pin": adminPIN}, 400)
	admin.must("POST", "/api/v1/setup", map[string]any{"username": adminUser, "password": adminPass, "pin": "pins"}, 400)
	resp, data = admin.do("POST", "/api/v1/setup", map[string]any{"username": adminUser, "password": adminPass, "pin": adminPIN}, false)
	check(resp != nil && resp.StatusCode == 201, "POST /api/v1/setup -> %d (want 201)", resp.StatusCode)
	if resp != nil && resp.StatusCode == 201 {
		admin.withCSRF(resp.Header.Get("X-CSRF-Token"))
	}
	check(bodyContains(data, `"role":"admin"`), "setup creates an administrator")
	admin.must("POST", "/api/v1/setup", map[string]any{"username": adminUser, "password": adminPass, "pin": adminPIN}, 409)
	admin.must("GET", "/api/v1/me", nil, 200)

	fmt.Println("CSRF enforcement")
	admin.withoutCSRF("PUT", "/api/v1/settings", nil, 403)

	fmt.Println("user management and authorization fencing")
	admin.must("GET", "/api/v1/users", nil, 200)
	admin.must("POST", "/api/v1/users", map[string]any{"username": bobUser, "password": "short", "pin": bobPIN}, 400)
	admin.must("POST", "/api/v1/users", map[string]any{"username": adminUser, "password": bobPass, "pin": bobPIN}, 409)
	admin.must("POST", "/api/v1/users", map[string]any{"username": bobUser, "password": bobPass, "pin": bobPIN, "role": "user"}, 201)
	bob := newSess(base)
	bob.login(bobUser, bobPass, "")
	bob.must("GET", "/api/v1/users", nil, 403)
	bob.must("GET", "/api/v1/sip", nil, 403)
	bob.must("PUT", "/api/v1/sip", map[string]any{"domain": "sip.example.com"}, 403)
	bob.must("GET", "/api/v1/accounts", nil, 200)
	listUsers := admin.must("GET", "/api/v1/users", nil, 200)
	bobID := ""
	if bodyContains(listUsers, bobUser) {
		var users []map[string]any
		_ = json.Unmarshal(listUsers, &users)
		for _, u := range users {
			if u["username"] == bobUser {
				bobID, _ = u["id"].(string)
			}
		}
	}
	check(bobID != "", "new user appears in the admin user list")
	admin.must("DELETE", "/api/v1/users/"+bobID, nil, 204)
	bob.must("GET", "/api/v1/me", nil, 401)
	me := admin.must("GET", "/api/v1/me", nil, 200)
	adminID := ""
	var m map[string]any
	_ = json.Unmarshal(me, &m)
	adminID, _ = m["id"].(string)
	admin.must("DELETE", "/api/v1/users/"+adminID, nil, 400)

	fmt.Println("sign-in failures and brute-force guard")
	bad := newSess(base)
	bad.must("POST", "/api/v1/login", map[string]any{"username": adminUser, "password": "wrong-password-xx"}, 401)
	loginAgain := newSess(base)
	loginAgain.login(adminUser, adminPass, "")
	bruteforce(base, "nonexistent-user", "x", []int{401, 401, 401, 401, 401, 429})

	fmt.Println("voice and alert settings")
	got := admin.must("GET", "/api/v1/settings", nil, 200)
	check(bodyContains(got, `"menu_speed":3`) && bodyContains(got, `"email_speed":2`), "default voice settings present")

	fmt.Println("outbound alert call path")
	resp, data = admin.do("POST", "/api/v1/alerts/test", nil, true)
	check(bodyContains(data, "alert phone"), "alert test refused without a phone number (-> %d)", resp.StatusCode)

	admin.must("PUT", "/api/v1/settings", map[string]any{"tts_voice": "../escape", "menu_speed": 3, "email_speed": 2}, 400)
	admin.must("PUT", "/api/v1/settings", map[string]any{"tts_voice": "en_US-hfc_male-medium", "menu_speed": 99, "email_speed": 99, "alerts_enabled": true, "alert_phone": "sip:" + phone + "@example.com"}, 200)
	got = admin.must("GET", "/api/v1/settings", nil, 200)
	check(bodyContains(got, `"menu_speed":3`), "out-of-range menu speed is clamped")
	check(bodyContains(got, `"alert_phone":"`+phone+`"`), "alert phone is normalized to the stable telephone identity")
	resp, data = admin.do("POST", "/api/v1/alerts/test", nil, true)
	check(resp.StatusCode == 400 && bodyContains(data, "SIP registrar"), "alert test refused without an SIP registrar (%d)", resp.StatusCode)
	admin.must("PUT", "/api/v1/sip", map[string]any{"domain": "sip.example.com", "username": "voxmail", "password": "secret", "port": 5060, "transport": "udp", "reg_interval": 300, "enabled": false}, 200)
	time.Sleep(2 * time.Second) // allow the bridge to reconnect to baresip
	admin.must("POST", "/api/v1/alerts/test", nil, 200)

	fmt.Println("SIP settings validation")
	admin.must("GET", "/api/v1/sip", nil, 200)
	admin.must("PUT", "/api/v1/sip", map[string]any{"domain": "sip.example.com", "username": "voxmail", "port": 0}, 400)
	admin.must("PUT", "/api/v1/sip", map[string]any{"domain": "sip.example.com", "username": "voxmail", "port": 70000}, 400)
	admin.must("PUT", "/api/v1/sip", map[string]any{"domain": "bad host!", "username": "voxmail"}, 400)
	admin.must("PUT", "/api/v1/sip", map[string]any{"domain": "sip.example.com", "username": "", "enabled": true}, 400)
	admin.must("PUT", "/api/v1/sip", map[string]any{"domain": "sip.example.com", "username": "voxmail", "reg_interval": 999999}, 400)
	sip := admin.must("GET", "/api/v1/sip", nil, 200)
	check(bodyContains(sip, `"domain":"sip.example.com"`) && bodyContains(sip, `"password_set":true`), "SIP domain and sealed password persist")

	fmt.Println("mail account validation and isolation")
	admin.must("POST", "/api/v1/accounts", map[string]any{"canonical_name": "Work", "email": "a@example.com", "imap_host": "imap.example.com", "imap_user": "a", "smtp_host": "smtp.example.com", "smtp_user": "a"}, 400)
	admin.must("POST", "/api/v1/accounts", acct("bad-email", "imap.example.com", 993, "a", "pw", nil, nil), 400)
	admin.must("POST", "/api/v1/accounts", acct("Work", "imap.example.com", 70000, "a", "pw", &map[string]string{}, &[]string{}), 400)
	admin.must("POST", "/api/v1/accounts", acct("Work", "imap.example.com", 993, "a", "pw", &map[string]string{"INBOX": ".."}, &[]string{}), 400)
	admin.must("POST", "/api/v1/accounts", acct("Work", "imap.example.com", 993, "a", "pw", &map[string]string{"a": "X", "b": "X"}, &[]string{}), 400)
	admin.must("POST", "/api/v1/accounts", acct("Work", "imap.example.com", 993, "a", "pw", &map[string]string{}, &[]string{"bad\nfolder"}), 400)
	cutoff := "not-a-timestamp"
	admin.must("POST", "/api/v1/accounts", acct("Work", "imap.example.com", 993, "a", "pw", &map[string]string{}, &[]string{}, &cutoff), 400)
	data = admin.must("POST", "/api/v1/accounts", acct("Work Mail", "imap.example.com", 993, "a", "pw", &map[string]string{"INBOX": "Inbox"}, &[]string{"INBOX"}), 201)
	accountID := ""
	var created map[string]string
	_ = json.Unmarshal(data, &created)
	accountID = created["id"]
	check(accountID != "", "account is created with a stable id")
	got = admin.must("GET", "/api/v1/accounts", nil, 200)
	check(bodyContains(got, `"canonical_name":"Work Mail"`), "account is listed for its owner")
	check(!bodyContains(got, "imap_password") && !bodyContains(got, "smtp_password"), "account views never expose passwords")
	probe := admin.must("POST", "/api/v1/accounts", map[string]any{
		"canonical_name": "Probe", "email": "probe@example.com",
		"imap_host": "127.0.0.1", "imap_port": 9, "imap_user": "a", "imap_password": "pw",
		"smtp_host": "smtp.example.com", "smtp_port": 465, "smtp_user": "a", "smtp_password": "pw",
		"folder_map": map[string]string{}, "alert_folders": []string{},
	}, 201)
	var probeResult map[string]string
	_ = json.Unmarshal(probe, &probeResult)
	admin.must("POST", "/api/v1/accounts/test", map[string]any{"account_id": probeResult["id"]}, 502)
	admin.must("DELETE", "/api/v1/accounts/"+accountID, nil, 204)
	admin.must("DELETE", "/api/v1/accounts/"+probeResult["id"], nil, 204)

	bob2 := newSess(base)
	bob2.login(bobUser, bobPass, "")
	recreated := admin.must("POST", "/api/v1/accounts", acct("Jane Mail", "imap.example.com", 993, "j", "pw", &map[string]string{}, &[]string{}), 201)
	recreatedID := ""
	var rcreated map[string]string
	_ = json.Unmarshal(recreated, &rcreated)
	recreatedID = rcreated["id"]
	bob2.must("POST", "/api/v1/accounts/test", map[string]any{"account_id": recreatedID}, 404)
	got = bob2.must("GET", "/api/v1/accounts", nil, 200)
	check(!bodyContains(got, "Jane Mail"), "another user cannot see a foreign account")
	bob2.must("DELETE", "/api/v1/accounts/"+recreatedID, nil, 204)
	got = admin.must("GET", "/api/v1/accounts", nil, 200)
	check(bodyContains(got, "Jane Mail"), "foreign delete is a no-op for the real owner")

	fmt.Println("contacts")
	admin.must("POST", "/api/v1/contacts", map[string]any{"name": "", "email": "x@example.com"}, 400)
	admin.must("POST", "/api/v1/contacts", map[string]any{"name": "Peach", "email": "peach@example.com"}, 201)
	admin.must("POST", "/api/v1/contacts", map[string]any{"name": "Peach", "email": "peach@example.com"}, 409)
	created = map[string]string{}
	data = admin.must("POST", "/api/v1/contacts", map[string]any{"name": "Toad", "email": "toad@example.com"}, 201)
	_ = json.Unmarshal(data, &created)
	contactID := created["id"]
	admin.must("PUT", "/api/v1/contacts/"+contactID, map[string]any{"name": "Toadstool", "email": "toad@example.com"}, 200)
	got = admin.must("GET", "/api/v1/contacts", nil, 200)
	check(bodyContains(got, "Peach") && bodyContains(got, "Toadstool"), "contacts are listed after create/update")
	check(!bodyContains(bob2.must("GET", "/api/v1/contacts", nil, 200), "Peach"), "another user cannot see foreign contacts")
	admin.must("DELETE", "/api/v1/contacts/"+contactID, nil, 204)
	admin.must("PUT", "/api/v1/contacts/"+contactID, map[string]any{"name": "Zombie", "email": "z@example.com"}, 400)

	fmt.Println("caller whitelist")
	admin.must("POST", "/api/v1/whitelist", map[string]any{"phone": ""}, 400)
	admin.must("POST", "/api/v1/whitelist", map[string]any{"phone": "sip:" + phone + "@provider.example;transport=udp"}, 201)
	admin.must("POST", "/api/v1/whitelist", map[string]any{"phone": phone}, 409)
	got = admin.must("GET", "/api/v1/whitelist", nil, 200)
	check(bodyContains(got, phone), "whitelist is normalized and lists the entry")
	check(!bodyContains(bob2.must("GET", "/api/v1/whitelist", nil, 200), phone), "another user cannot see foreign whitelist entries")
	var entries []map[string]any
	_ = json.Unmarshal(got, &entries)
	entryID := ""
	for _, e := range entries {
		if e["phone"] == phone {
			entryID = fmt.Sprintf("%v", e["id"])
		}
	}
	admin.must("DELETE", "/api/v1/whitelist/"+entryID, nil, 204)
	admin.must("DELETE", "/api/v1/whitelist/not-a-number", nil, 400)

	fmt.Println("console and security headers")
	req, _ := http.NewRequest("GET", base+"/", nil)
	page, err := newSess(base).c.Do(req)
	if err != nil {
		fatalf("GET /: %v", err)
	} else {
		head := page.Header
		html, _ := io.ReadAll(page.Body)
		page.Body.Close()
		check(page.StatusCode == 200 && strings.Contains(string(html), "<form"), "console page is served")
		check(head.Get("X-Content-Type-Options") == "nosniff" && head.Get("X-Frame-Options") == "DENY", "security headers are present")
	}
}

func afterRestart(base string) {
	fmt.Println("post-restart liveness and setup guard")
	check(waitReady(base, 90*time.Second), "container is ready after restart (/readyz)")
	_, info := newSess(base).do("GET", "/api/v1", nil, false)
	check(bodyContains(info, `"setup_available":false`), "first-run setup stays completed across restart")
	admin := newSess(base)
	admin.must("POST", "/api/v1/setup", map[string]any{"username": adminUser, "password": adminPass, "pin": adminPIN}, 409)

	fmt.Println("session, settings, and data persistence")
	admin.login(adminUser, adminPass, "")
	sip := admin.must("GET", "/api/v1/sip", nil, 200)
	check(bodyContains(sip, `"domain":"sip.example.com"`) && bodyContains(sip, `"password_set":true`), "SIP settings survive restart")
	settings := admin.must("GET", "/api/v1/settings", nil, 200)
	check(bodyContains(settings, `"alert_phone":"`+phone+`"`), "per-user settings survive restart")
	accounts := admin.must("GET", "/api/v1/accounts", nil, 200)
	check(bodyContains(accounts, "Jane Mail"), "mail accounts survive restart")
	contacts := admin.must("GET", "/api/v1/contacts", nil, 200)
	check(bodyContains(contacts, "Peach"), "contacts survive restart")
	users := admin.must("GET", "/api/v1/users", nil, 200)
	check(bodyContains(users, bobUser), "users survive restart")

	fmt.Println("alert path after restart")
	time.Sleep(2 * time.Second)
	admin.must("POST", "/api/v1/alerts/test", nil, 200)
}

// acct builds a full account payload; optional pointers allow invalid fields.
func acct(name, host string, port int, user, password string, folderMap *map[string]string, alertFolders *[]string, cutoff ...*string) map[string]any {
	fm := map[string]string{}
	af := []string{}
	var initial *string
	if len(cutoff) > 0 {
		initial = cutoff[0]
	}
	if folderMap != nil {
		fm = *folderMap
	}
	if alertFolders != nil {
		af = *alertFolders
	}
	return map[string]any{
		"canonical_name":        name,
		"email":                 "a@" + host,
		"sender_name":           name,
		"imap_host":             host,
		"imap_user":             user,
		"imap_password":         password,
		"smtp_host":             host,
		"smtp_user":             user,
		"smtp_password":         password,
		"imap_port":             port,
		"smtp_port":             port,
		"folder_map":            fm,
		"alert_folders":         af,
		"sync_interval_minutes": 5,
		"display_order":         0,
		"initial_cutoff":        initial,
	}
}
