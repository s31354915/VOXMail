// Package alerts detects new mail in alert-enabled accounts and places
// outgoing calls that announce it on the subscriber's phone number. The
// caller is never admitted through the IVR: the far end answers, hears a
// summary, and the call hangs up. An in-memory per-user delay prevents a
// flood of calls when many messages arrive at once.
package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/calls"
	"github.com/voxmail/voxmail/internal/store"
)

// Bridge dials outgoing calls through the long-lived baresip connection.
type Bridge interface {
	Dial(context.Context, calls.DialRequest) error
}

type Service struct {
	Store       *store.Store
	Bridge      Bridge
	Log         *slog.Logger
	Interval    time.Duration
	MinDelay    time.Duration
	MaxPerRound int

	mu   sync.Mutex
	last map[string]time.Time
}

func (s *Service) Run(ctx context.Context) error {
	if s.Store == nil || s.Bridge == nil {
		return fmt.Errorf("alerts store and bridge are required")
	}
	interval := s.Interval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	s.mu.Lock()
	if s.last == nil {
		s.last = make(map[string]time.Time)
	}
	s.mu.Unlock()
	for {
		if err := s.round(ctx); err != nil && s.Log != nil {
			s.Log.Error("alerts round failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (s *Service) round(ctx context.Context) error {
	candidates, err := s.Store.PendingAlerts(ctx)
	if err != nil {
		return err
	}
	domain := s.targetDomain(ctx)
	if domain == "" {
		if s.Log != nil {
			s.Log.Warn("alert round skipped: no SIP registrar/domain configured")
		}
		return nil
	}
	byUser := make(map[string][]store.AlertCandidate)
	for _, candidate := range candidates {
		if alertFolderMatch(candidate) {
			byUser[candidate.UserID] = append(byUser[candidate.UserID], candidate)
		}
	}
	minDelay := s.MinDelay
	if minDelay <= 0 {
		minDelay = 5 * time.Minute
	}
	max := s.MaxPerRound
	if max <= 0 {
		max = 1
	}
	if s.last == nil {
		s.last = make(map[string]time.Time)
	}
	now := time.Now()
	var claimed []int64
	for userID, list := range byUser {
		s.mu.Lock()
		last := s.last[userID]
		s.mu.Unlock()
		if !last.IsZero() && now.Sub(last) < minDelay {
			continue
		}
		if len(list) > max {
			list = list[:max]
		}
		uri := dialURI(list[0].Phone, domain)
		if uri == "" {
			if s.Log != nil {
				s.Log.Warn("alert skipped: no dial target", "user_id", userID)
			}
			continue
		}
		err := s.Bridge.Dial(ctx, calls.DialRequest{URI: uri, UserID: userID, AccountID: list[0].AccountID, Text: alertText(list)})
		if err != nil {
			if s.Log != nil {
				s.Log.Warn("alert dial failed", "user_id", userID, "error", err)
			}
			continue
		}
		s.mu.Lock()
		s.last[userID] = now
		s.mu.Unlock()
		for _, candidate := range list {
			claimed = append(claimed, candidate.MessageID)
		}
		if s.Log != nil {
			s.Log.Info("alert call placed", "user_id", userID, "uri", uri, "messages", len(list))
		}
	}
	return s.Store.MarkAlertsNotified(ctx, claimed)
}

func (s *Service) targetDomain(ctx context.Context) string {
	settings, err := s.Store.GetSIP(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(settings.Domain)
}

// TestAlert dials the user's alert number with a canned message so the web
// console can verify the outbound path end to end. It bypasses the pending
// mail scan and never claims any message.
func (s *Service) TestAlert(ctx context.Context, userID string) error {
	if s.Store == nil || s.Bridge == nil {
		return fmt.Errorf("the call service is not running")
	}
	var phone string
	if err := s.Store.DB.QueryRowContext(ctx, `SELECT COALESCE(alert_phone,'') FROM settings WHERE user_id = ?`, userID).Scan(&phone); err != nil {
		return fmt.Errorf("alert settings are unavailable")
	}
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return fmt.Errorf("no alert phone number is configured for this user")
	}
	domain := s.targetDomain(ctx)
	if domain == "" {
		return fmt.Errorf("an SIP registrar/domain must be configured to place alert calls")
	}
	uri := dialURI(phone, domain)
	req := calls.DialRequest{
		URI:    uri,
		UserID: userID,
		Text:   "This is a test alert call from VOXMail. Your call alert settings are working.",
	}
	if err := s.Bridge.Dial(ctx, req); err != nil {
		return fmt.Errorf("the test alert call could not be placed: %w", err)
	}
	if s.Log != nil {
		s.Log.Info("test alert call placed", "user_id", userID, "uri", uri)
	}
	return nil
}

// alertFolderMatch reports whether the message landed in one of the folders
// the account owner selected for call alerts. An empty folder list never
// matches.
func alertFolderMatch(candidate store.AlertCandidate) bool {
	var folders []string
	if err := json.Unmarshal([]byte(candidate.AlertFolders), &folders); err != nil {
		return false
	}
	if len(folders) == 0 {
		return false
	}
	for _, folder := range folders {
		if strings.EqualFold(strings.TrimSpace(folder), strings.TrimSpace(candidate.Folder)) {
			return true
		}
	}
	return false
}

// dialURI builds a SIP request-URI for an alert call. A stored phone number
// that is already a full SIP URI passes through untouched; otherwise the SIP
// account domain is appended so baresip's account can complete the call.
func dialURI(phone, domain string) string {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(phone), "sip:") {
		return phone
	}
	if domain == "" {
		return "sip:" + phone
	}
	return "sip:" + phone + "@" + domain
}

func alertText(list []store.AlertCandidate) string {
	if len(list) == 1 {
		c := list[0]
		return "You have new email in " + describe(c)
	}
	c := list[0]
	return fmt.Sprintf("You have %d new email messages. The latest is in %s.", len(list), describe(c))
}

func describe(c store.AlertCandidate) string {
	sender := strings.TrimSpace(c.Sender)
	if sender == "" {
		sender = "an unknown sender"
	}
	subject := strings.TrimSpace(c.Subject)
	if subject == "" {
		subject = "with no subject line"
	}
	return fmt.Sprintf("%s. From %s. Subject %s.", strings.TrimSpace(c.Folder), sender, subject)
}