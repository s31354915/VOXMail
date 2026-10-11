package mailsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/voxmail/voxmail/internal/mailparse"
	"github.com/voxmail/voxmail/internal/store"
)

// alignConfirmedRead updates only cache copies of this exact provider UID/epoch.
// Separate provider folders and same-Message-ID messages are never combined.
func (s *Service) alignConfirmedRead(ctx context.Context, account store.Account, remote string, action store.QueuedReadMutation) (bool, error) {
	// Isolated protocol unit tests can omit the application's data root.
	if s.Root == "" {
		return false, nil
	}
	release := s.Store.LockMailReadState()
	defer release()
	folders, err := s.Store.PhysicalFoldersForRemote(ctx, account.UserID, account.ID, remote)
	if err != nil {
		return false, err
	}
	latest, err := s.Store.LatestReadForIdentity(ctx, account.ID, folders, action.UID, action.UIDValidity)
	if err != nil {
		return false, err
	}
	// A newer action during the network call must keep its own cache flags.
	if latest.MutationID != action.MutationID {
		return false, nil
	}
	copies, err := s.Store.ReadIdentityCopies(ctx, account.ID, folders, action.UID, action.UIDValidity)
	if err != nil {
		return false, err
	}
	root := filepath.Join(s.Root, "mail", account.ID)
	for _, copy := range copies {
		if !underSyncRoot(copy.Path, root) || !pathWithinExistingRoot(copy.Path, root) {
			return false, fmt.Errorf("confirmed read cache path is outside account root")
		}
		target, err := mailparse.SetMaildirRead(copy.Path, action.Read)
		if err != nil {
			return false, err
		}
		if err := s.Store.MarkConfirmedReadAtPath(ctx, copy, action.Read, target); err != nil {
			if target != copy.Path {
				_ = os.Rename(target, copy.Path)
			}
			return false, err
		}
	}
	return true, nil
}
