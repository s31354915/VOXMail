package store

import (
	"context"
	"fmt"
	"time"

	"github.com/voxmail/voxmail/internal/secret"
)

func (s *Store) GetSIP(ctx context.Context) (SIPSettings, error) {
	var st SIPSettings
	var enabled int
	err := s.DB.QueryRowContext(ctx, `SELECT domain,username,password,port,local_port,registrar_port,transport,reg_interval,enabled FROM sip_settings WHERE id=1`).Scan(&st.Domain, &st.Username, &st.Password, &st.Port, &st.LocalPort, &st.RegistrarPort, &st.Transport, &st.RegInterval, &enabled)
	if err != nil {
		return SIPSettings{}, err
	}
	if st.LocalPort == 0 {
		st.LocalPort = st.Port
	}
	if st.RegistrarPort == 0 {
		st.RegistrarPort = st.Port
	}
	st.Port = st.RegistrarPort
	st.Enabled = enabled != 0
	return st, nil
}

// UpsertSIP persists the deployment SIP account. When storage.Password is
// blank the previously sealed password is preserved; otherwise the plaintext
// is sealed before writing.
func (s *Store) UpsertSIP(ctx context.Context, box *secret.Box, storage SIPSettings) error {
	if box == nil {
		return fmt.Errorf("secret box is required")
	}
	if storage.LocalPort == 0 {
		storage.LocalPort = storage.Port
	}
	if storage.RegistrarPort == 0 {
		storage.RegistrarPort = storage.Port
	}
	if storage.LocalPort == 0 {
		storage.LocalPort = 5060
	}
	if storage.RegistrarPort == 0 {
		storage.RegistrarPort = 5060
	}
	storage.Port = storage.RegistrarPort
	if storage.Transport == "" {
		storage.Transport = "udp"
	}
	if storage.RegInterval == 0 {
		storage.RegInterval = 300
	}
	sealed := storage.Password
	if sealed == "" {
		_ = s.DB.QueryRowContext(ctx, `SELECT password FROM sip_settings WHERE id=1`).Scan(&sealed)
	}
	if storage.Password != "" {
		value, err := box.Seal(storage.Password)
		if err != nil {
			return err
		}
		sealed = value
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sip_settings(id,domain,username,password,port,local_port,registrar_port,transport,reg_interval,enabled,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET domain=excluded.domain,username=excluded.username,password=excluded.password,port=excluded.port,local_port=excluded.local_port,registrar_port=excluded.registrar_port,transport=excluded.transport,reg_interval=excluded.reg_interval,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		storage.Domain, storage.Username, sealed, storage.Port, storage.LocalPort, storage.RegistrarPort, storage.Transport, storage.RegInterval, storage.Enabled, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
