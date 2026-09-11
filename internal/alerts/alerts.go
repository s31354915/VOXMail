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

	mu       sync.Mutex
	last     map[string]time.Time
	inflight map[int64]bool
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
	if s.inflight == nil {
		s.inflight = make(map[int64]bool)
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
		s.mu.Lock()
		inflight := s.inflight[candidate.MessageID]
		s.mu.Unlock()
		if !inflight && alertFolderMatch(candidate) {
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
	s.mu.Lock()
	if s.last == nil {
		s.last = make(map[string]time.Time)
	}
	if s.inflight == nil {
		s.inflight = make(map[int64]bool)
	}
	s.mu.Unlock()
	now := time.Now()
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
		ids := make([]int64, 0, len(list))
		for _, candidate := range list {
			ids = append(ids, candidate.MessageID)
		}
		claimed, claimErr := s.Store.ClaimAlertMessages(ctx, userID, ids)
		if claimErr != nil {
			if s.Log != nil {
				s.Log.Warn("alert claim failed", "user_id", userID, "error", claimErr)
			}
			continue
		}
		if len(claimed) == 0 {
			continue
		}
		claimedSet := make(map[int64]bool, len(claimed))
		for _, id := range claimed {
			claimedSet[id] = true
		}
		filtered := list[:0]
		for _, candidate := range list {
			if claimedSet[candidate.MessageID] {
				filtered = append(filtered, candidate)
			}
		}
		list = filtered
		ids = claimed
		done := make(chan bool, 1)
		s.mu.Lock()
		for _, id := range ids {
			s.inflight[id] = true
		}
		s.mu.Unlock()
		err := s.Bridge.Dial(ctx, calls.DialRequest{URI: uri, UserID: userID, AccountID: list[0].AccountID, Text: alertText(list), Done: done})
		if err != nil {
			s.clearInflight(ids)
			_ = s.Store.ReleaseAlertClaims(context.Background(), userID, ids)
			if s.Log != nil {
				s.Log.Warn("alert dial failed", "user_id", userID, "error", err)
			}
			continue
		}
		s.mu.Lock()
		s.last[userID] = now
		s.mu.Unlock()
		go s.finishAlert(ctx, userID, ids, done)
		if s.Log != nil {
			s.Log.Info("alert call placed", "user_id", userID, "uri", uri, "messages", len(list))
		}
	}
	return nil
}

func (s *Service) clearInflight(ids []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.inflight, id)
	}
}

func (s *Service) finishAlert(ctx context.Context, userID string, ids []int64, done <-chan bool) {
	success := false
	select {
	case success = <-done:
	case <-ctx.Done():
	}
	s.clearInflight(ids)
	if success {
		if err := s.Store.MarkAlertMessagesNotified(context.Background(), userID, ids); err != nil && s.Log != nil {
			s.Log.Warn("could not mark alert messages notified", "error", err)
		}
	} else if err := s.Store.ReleaseAlertClaims(context.Background(), userID, ids); err != nil && s.Log != nil {
		s.Log.Warn("could not release failed alert claims", "error", err)
	}
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
	phone, err := s.Store.ActiveAlertNumber(ctx, userID)
	if err != nil {
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
