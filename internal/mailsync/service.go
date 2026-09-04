package mailsync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/voxmail/voxmail/internal/mailconfig"
	"github.com/voxmail/voxmail/internal/mailindex"
	"github.com/voxmail/voxmail/internal/store"
)

type Service struct {
	Store  *store.Store
	Root   string
	Runner Runner
	Log    *slog.Logger
	Index  *mailindex.Indexer
	mu     sync.Mutex
	busy   map[string]bool
	last   map[string]time.Time
}

func (s *Service) Run(ctx context.Context) error {
	if s.Store == nil || s.Root == "" {
		return fmt.Errorf("sync store and root are required")
	}
	s.busy = make(map[string]bool)
	s.last = make(map[string]time.Time)
	_ = s.SyncAll(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = s.SyncDue(ctx)
		}
	}
}

func (s *Service) SyncAll(ctx context.Context) error {
	accounts, err := s.Store.ListAllAccounts(ctx)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if err := s.syncAccount(ctx, account); err != nil && s.Log != nil {
			s.Log.Error("mail sync failed", "account", account.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) SyncDue(ctx context.Context) error {
	accounts, err := s.Store.ListAllAccounts(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, account := range accounts {
		interval := time.Duration(account.SyncIntervalMinutes) * time.Minute
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		s.mu.Lock()
		due := now.Sub(s.last[account.ID]) >= interval
		s.mu.Unlock()
		if due {
			if err := s.syncAccount(ctx, account); err != nil && s.Log != nil {
				s.Log.Error("mail sync failed", "account", account.ID, "error", err)
			}
		}
	}
	return nil
}

func (s *Service) syncAccount(ctx context.Context, account store.Account) error {
	s.mu.Lock()
	if s.busy[account.ID] {
		s.mu.Unlock()
		return nil
	}
	s.busy[account.ID] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.busy, account.ID); s.mu.Unlock() }()
	root := filepath.Join(s.Root, "mail", account.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	configText, err := mailconfig.Generate(mailconfig.Account{ID: account.ID, IMAPHost: account.IMAPHost, IMAPPort: account.IMAPPort, IMAPUser: account.IMAPUser, MaildirRoot: root})
	if err != nil {
		return err
	}
	configDir := filepath.Join(s.Root, "config", "mbsync")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	configPath := filepath.Join(configDir, account.ID+".conf")
	tmp, err := os.CreateTemp(configDir, ".conf-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(configText); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, configPath); err != nil {
		return err
	}
	result, err := s.Runner.Sync(ctx, configPath, account.ID)
	if err != nil {
		return err
	}
	if result.Changed && s.Index != nil {
		if err := s.Index.Index(ctx, account.ID, root); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.last[account.ID] = time.Now()
	s.mu.Unlock()
	if result.Changed && s.Log != nil {
		s.Log.Info("mail sync changed maildir", "account", account.ID)
	}
	return nil
}
