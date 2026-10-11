package mailsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	remoteimap "github.com/voxmail/voxmail/internal/imap"
	"github.com/voxmail/voxmail/internal/store"
)

type readMutationConnection interface {
	SetSeenByUID(string, uint32, uint32, bool) error
	ReconcileSeenByUID(string, uint32, uint32, bool) (remoteimap.MutationState, error)
	Close() error
}

func (s *Service) retryRetainedReads(ctx context.Context, account store.Account, watermark int64, folders []string, address string) error {
	// A wildcard run already includes all local directories. Production uses an
	// authenticated explicit plan, so historical directories need direct retries.
	if folders == nil {
		return nil
	}
	pending, err := s.Store.QueuedReadMutations(ctx, account.ID, watermark)
	if err != nil {
		return err
	}
	covered := make(map[string]bool, len(folders))
	for _, folder := range folders {
		covered[folder] = true
	}
	retained := pending[:0]
	for _, item := range pending {
		if !covered[item.Folder] || item.Read != item.CacheRead {
			retained = append(retained, item)
		}
	}
	if len(retained) == 0 {
		return nil
	}
	open := func() (readMutationConnection, error) {
		if s.Secrets == nil {
			return nil, fmt.Errorf("IMAP credentials unavailable for retained read changes")
		}
		password, err := s.Secrets.Open(account.IMAPPassword)
		if err != nil {
			return nil, err
		}
		return remoteimap.Open(ctx, remoteimap.Config{Host: account.IMAPHost, Address: address, Port: account.IMAPPort, Security: account.IMAPSecurity, Username: account.IMAPUser, Password: password})
	}
	return s.confirmRetainedReads(ctx, account, retained, open)
}

func (s *Service) confirmRetainedReads(ctx context.Context, account store.Account, pending []store.QueuedReadMutation, open func() (readMutationConnection, error)) error {
	var connection readMutationConnection
	defer func() {
		if connection != nil {
			_ = connection.Close()
		}
	}()
	var failures []error
	for _, item := range pending {
		action := item
		var remote string
		attempt := func() error {
			// A remap never authorizes guessing among same-Message-ID copies.
			if item.UID == 0 || item.UIDValidity == 0 {
				return fmt.Errorf("%w: retained read change requires UID and UIDVALIDITY", remoteimap.ErrMissingIdentity)
			}
			folder, err := s.Store.RemoteFolderForLocal(ctx, account.UserID, account.ID, item.Folder)
			if err != nil {
				return err
			}
			remote = folder
			physical, err := s.Store.PhysicalFoldersForRemote(ctx, account.UserID, account.ID, folder)
			if err != nil {
				return err
			}
			latest, err := s.Store.LatestReadForIdentity(ctx, account.ID, physical, item.UID, item.UIDValidity)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && latest.MutationID > action.MutationID {
				action = latest
			}
			if connection == nil {
				connection, err = open()
				if err != nil {
					return err
				}
			}
			err = connection.SetSeenByUID(folder, item.UID, item.UIDValidity, action.Read)
			if !errors.Is(err, remoteimap.ErrMutationUncertain) {
				return err
			}
			_ = connection.Close()
			connection = nil
			// Reconnect solely to confirm the exact attempted identity. Do not replay
			// an uncertain write or substitute another message identity.
			connection, err = open()
			if err != nil {
				return fmt.Errorf("%w: retained read confirmation connection: %v", remoteimap.ErrMutationUncertain, err)
			}
			state, err := connection.ReconcileSeenByUID(folder, item.UID, item.UIDValidity, action.Read)
			if err != nil {
				return err
			}
			if state == remoteimap.MutationStateApplied {
				return nil
			}
			if state == remoteimap.MutationStateNotApplied {
				return remoteimap.ErrMutationNotApplied
			}
			return remoteimap.ErrMutationUncertain
		}
		mutationErr := attempt()
		if mutationErr == nil {
			aligned, err := s.alignConfirmedRead(ctx, account, remote, action)
			if err != nil {
				mutationErr = err
			} else if aligned {
				item.Read = action.Read
			}
		}
		if action.MutationID != item.MutationID {
			if err := s.Store.FinishQueuedRead(ctx, action, mutationErr); err != nil {
				failures = append(failures, err)
			}
		}
		if err := s.Store.FinishQueuedRead(ctx, item, mutationErr); err != nil {
			failures = append(failures, err)
		}
		if mutationErr != nil {
			failures = append(failures, fmt.Errorf("retained read change %d: %w", item.MutationID, mutationErr))
			// A failed transport must not be reused for subsequent queued changes.
			if connection != nil {
				_ = connection.Close()
				connection = nil
			}
		}
	}
	return errors.Join(failures...)
}
