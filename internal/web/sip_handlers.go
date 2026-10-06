package web

import (
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/voxmail/voxmail/internal/store"
)

func (s *Server) sipSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	_ = u
	st, err := s.Store.GetSIP(r.Context())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		serverError(w, err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		st = store.SIPSettings{Port: 5060, LocalPort: 5060, RegistrarPort: 5060, Transport: "udp", RegInterval: 300}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"domain":         st.Domain,
		"username":       st.Username,
		"port":           st.Port,
		"local_port":     st.LocalPort,
		"registrar_port": st.RegistrarPort,
		"transport":      st.Transport,
		"reg_interval":   st.RegInterval,
		"enabled":        st.Enabled,
		"password_set":   st.Password != "",
	})
}

func (s *Server) saveSIP(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var body struct {
		Domain        string `json:"domain"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		Port          int    `json:"port"`
		LocalPort     int    `json:"local_port"`
		RegistrarPort int    `json:"registrar_port"`
		Transport     string `json:"transport"`
		RegInterval   int    `json:"reg_interval"`
		Enabled       bool   `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Domain = strings.TrimSpace(body.Domain)
	body.Username = strings.TrimSpace(body.Username)
	if body.LocalPort == 0 {
		body.LocalPort = body.Port
	}
	if body.RegistrarPort == 0 {
		body.RegistrarPort = body.Port
	}
	if body.LocalPort == 0 {
		body.LocalPort = 5060
	}
	if body.RegistrarPort == 0 {
		body.RegistrarPort = 5060
	}
	if body.LocalPort < 1 || body.LocalPort > 65535 || body.RegistrarPort < 1 || body.RegistrarPort > 65535 {
		writeError(w, http.StatusBadRequest, "SIP local and registrar ports must be between 1 and 65535")
		return
	}
	switch body.Transport {
	case "udp", "tcp", "tls":
	default:
		body.Transport = "udp"
	}
	if body.RegInterval < 0 || body.RegInterval > 86400 {
		writeError(w, http.StatusBadRequest, "registration interval must be between 0 and 86400 seconds")
		return
	}
	if body.Enabled {
		if body.Domain == "" || body.Username == "" {
			writeError(w, http.StatusBadRequest, "domain and username are required to enable SIP")
			return
		}
		if !validSIPHost(body.Domain) {
			writeError(w, http.StatusBadRequest, "SIP domain is invalid")
			return
		}
		if !validSIPUsername(body.Username) {
			writeError(w, http.StatusBadRequest, "SIP username contains invalid characters")
			return
		}
	}
	st := store.SIPSettings{Domain: body.Domain, Username: body.Username, Password: body.Password, Port: body.RegistrarPort, LocalPort: body.LocalPort, RegistrarPort: body.RegistrarPort, Transport: body.Transport, RegInterval: body.RegInterval, Enabled: body.Enabled}
	var release func()
	if s.SIP != nil {
		if gate, ok := s.Calls.(SIPApplyGate); ok {
			var gateErr error
			release, gateErr = gate.BeginSIPApply(r.Context())
			if gateErr != nil {
				writeError(w, http.StatusConflict, "SIP settings cannot be applied while calls are active")
				return
			}
			defer release()
		}
	}
	if err := s.Store.UpsertSIP(r.Context(), s.Secrets, st); err != nil {
		serverError(w, err)
		return
	}
	if s.SIP != nil {
		// The Baresip supervisor owns its child under the application lifetime;
		// this request context only bounds the apply operation itself.
		if err := s.SIP.Apply(r.Context()); err != nil {
			s.log().Error("sip settings applied but baresip restart failed", "error", err)
			writeError(w, http.StatusInternalServerError, "SIP settings saved, but the call client could not be restarted")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func validSIPHost(host string) bool {
	if host == "" || strings.ContainsAny(host, " \t\r\n;/\x00\\\"") || strings.Contains(host, "..") {
		return false
	}
	ipHost := strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.Contains(host, ":") {
		return net.ParseIP(ipHost) != nil
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validSIPUsername(username string) bool {
	if username == "" {
		return false
	}
	for _, r := range username {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '+' && r != '-' && r != '.' && r != '_' && r != '~' {
			return false
		}
	}
	return true
}
