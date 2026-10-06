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
	"github.com/voxmail/voxmail/internal/lifecycle"
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
	// MaxConcurrent bounds alert calls that have been accepted by the bridge
	// but have not yet produced a terminal Done result. Zero uses the safe
	// default. The per-user limit remains one regardless of this setting.
	MaxConcurrent int

	mu            sync.Mutex
	last          map[string]time.Time
	inflight      map[int64]bool
	testLast      map[string]time.Time
	pending       int
	pendingByUser map[string]int
	finishWG      sync.WaitGroup
}

const defaultMaxConcurrentAlerts = 10

func (s *Service) reservePending(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	max := s.MaxConcurrent
	if max <= 0 {
		max = defaultMaxConcurrentAlerts
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingByUser == nil {
		s.pendingByUser = make(map[string]int)
	}
	if s.pending >= max || s.pendingByUser[userID] >= 1 {
		return false
	}
	s.pending++
	s.pendingByUser[userID]++
	return true
}

func (s *Service) releasePending(userID string) {
	userID = strings.TrimSpace(userID)
	s.mu.Lock()
	defer s.mu.Unlock()
	count := s.pendingByUser[userID]
	if count <= 0 {
		return
	}
	if s.pending > 0 {
		s.pending--
	}
	if count > 1 {
		s.pendingByUser[userID] = count - 1
	} else {
		delete(s.pendingByUser, userID)
	}
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
	if s.testLast == nil {
		s.testLast = make(map[string]time.Time)
	}
	if s.pendingByUser == nil {
		s.pendingByUser = make(map[string]int)
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
		if persistent, persistentErr := s.Store.AlertNextDial(ctx, userID); persistentErr == nil && persistent.After(now) && (last.IsZero() || persistent.After(last)) {
			last = persistent
		}
		if !last.IsZero() && now.Sub(last) < minDelay {
			continue
		}
		if !s.reservePending(userID) {
			if s.Log != nil {
				s.Log.Warn("alert skipped: pending call limit reached", "user_id", userID)
			}
			continue
		}
		if len(list) > max {
			list = list[:max]
		}
		uri := dialURI(list[0].Phone, domain)
		if uri == "" {
			s.releasePending(userID)
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
			s.releasePending(userID)
			if s.Log != nil {
				s.Log.Warn("alert claim failed", "user_id", userID, "error", claimErr)
			}
			continue
		}
		if len(claimed) == 0 {
			s.releasePending(userID)
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
		outcome := func(state calls.AlertOutcome) {
			s.logAlertOutcome(userID, ids, state)
		}
		err := s.Bridge.Dial(ctx, calls.DialRequest{URI: uri, UserID: userID, AccountID: list[0].AccountID, Text: alertText(list), Done: done, Outcome: outcome})
		if err != nil {
			s.releasePending(userID)
			s.clearInflight(ids)
			cleanupCtx, cleanupCancel := lifecycle.CleanupContext(ctx)
			_ = s.Store.ReleaseAlertClaims(cleanupCtx, userID, ids)
			cleanupCancel()
			if s.Log != nil {
				s.Log.Warn("alert dial failed", "user_id", userID, "error", err)
			}
			continue
		}
		s.mu.Lock()
		s.last[userID] = now
		s.mu.Unlock()
		cleanupCtx, cleanupCancel := lifecycle.CleanupContext(ctx)
		if err := s.Store.SetAlertNextDial(cleanupCtx, userID, now.Add(minDelay)); err != nil && s.Log != nil {
			s.Log.Warn("could not persist alert cooldown", "user_id", userID, "error", err)
		}
		cleanupCancel()
		s.finishWG.Add(1)
		go func() {
			defer s.finishWG.Done()
			s.finishAlert(ctx, userID, ids, done)
		}()
		if s.Log != nil {
			s.Log.Info("alert call placed", "user_id", userID, "messages", len(list))
		}
	}
	return nil
}

// Wait waits for alert completion callbacks to finish their durable claim
// cleanup. Call it after Run has returned and before closing the store.
func (s *Service) Wait() {
	if s == nil {
		return
	}
	s.finishWG.Wait()
}

func (s *Service) clearInflight(ids []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.inflight, id)
	}
}

func (s *Service) finishAlert(ctx context.Context, userID string, ids []int64, done <-chan bool) {
	defer s.releasePending(userID)
	success := false
	select {
	case success = <-done:
	case <-ctx.Done():
	}
	s.clearInflight(ids)
	cleanupCtx, cleanupCancel := lifecycle.CleanupContext(ctx)
	defer cleanupCancel()
	if success {
		if err := s.Store.MarkAlertMessagesNotified(cleanupCtx, userID, ids); err != nil && s.Log != nil {
			s.Log.Warn("could not mark alert messages notified", "error", err)
		}
	} else if err := s.Store.ReleaseAlertClaims(cleanupCtx, userID, ids); err != nil && s.Log != nil {
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
	minDelay := s.MinDelay
	if minDelay <= 0 {
		minDelay = 5 * time.Minute
	}
	now := time.Now()
	if next, err := s.Store.AlertNextDial(ctx, userID); err == nil && next.After(now) {
		return fmt.Errorf("an alert call was placed recently; try again later")
	}
	s.mu.Lock()
	if s.testLast == nil {
		s.testLast = make(map[string]time.Time)
	}
	if last := s.testLast[userID]; !last.IsZero() && now.Sub(last) < minDelay {
		s.mu.Unlock()
		return fmt.Errorf("an alert call was placed recently; try again later")
	}
	s.mu.Unlock()
	if !s.reservePending(userID) {
		return fmt.Errorf("too many alert calls are currently pending; try again later")
	}
	s.mu.Lock()
	s.testLast[userID] = now
	s.mu.Unlock()
	uri := dialURI(phone, domain)
	done := make(chan bool, 1)
	req := calls.DialRequest{
		URI:    uri,
		UserID: userID,
		Text:   "This is a test alert call from VOXMail. Your call alert settings are working.",
		Done:   done,
		Outcome: func(state calls.AlertOutcome) {
			s.logAlertOutcome(userID, nil, state)
		},
	}
	if err := s.Bridge.Dial(ctx, req); err != nil {
		s.releasePending(userID)
		s.mu.Lock()
		delete(s.testLast, userID)
		s.mu.Unlock()
		return fmt.Errorf("the test alert call could not be placed: %w", err)
	}
	s.finishWG.Add(1)
	go func() {
		defer s.finishWG.Done()
		select {
		case <-done:
		case <-ctx.Done():
		}
		s.releasePending(userID)
	}()
	cleanupCtx, cleanupCancel := lifecycle.CleanupContext(ctx)
	defer cleanupCancel()
	if err := s.Store.SetAlertNextDial(cleanupCtx, userID, now.Add(minDelay)); err != nil && s.Log != nil {
		s.Log.Warn("could not persist test alert cooldown", "user_id", userID, "error", err)
	}
	if s.Log != nil {
		s.Log.Info("test alert call placed", "user_id", userID)
	}
	return nil
}

func (s *Service) logAlertOutcome(userID string, messageIDs []int64, outcome calls.AlertOutcome) {
	if s.Log != nil {
		s.Log.Info("alert call outcome", "user_id", userID, "message_ids", messageIDs, "outcome", outcome)
	}
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
		return "You have new email in " + strings.TrimSpace(c.Folder) + "."
	}
	c := list[0]
	return fmt.Sprintf("You have %d new email messages. The latest is in %s.", len(list), strings.TrimSpace(c.Folder))
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
