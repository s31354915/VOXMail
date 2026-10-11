package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

// RecordFolderDiscovery retains provider LIST names independently of synchronized
// Maildir/UID metadata. Testing credentials must never overwrite actual paths.
// The version guard rejects discovery using settings changed during provider I/O.
func (s *Store) RecordFolderDiscovery(ctx context.Context, userID, accountID string, version int, folders []string) error {
	if len(folders) > 4096 {
		return fmt.Errorf("%w: too many discovered folders", ErrInvalidAccountConfig)
	}
	unique := make(map[string]string, len(folders))
	for _, remote := range folders {
		// Reuse the physical mapping boundary without conflating distinct remotes.
		mapping, err := json.Marshal(map[string]string{remote: "Discovered"})
		if err != nil {
			return err
		}
		if _, err := validateAccountFolderMap(string(mapping)); err != nil {
			return err
		}
		unique[folderpolicy.RemoteKey(remote)] = remote
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int
	err = tx.QueryRowContext(ctx, `SELECT config_version FROM accounts WHERE id=? AND user_id=? AND COALESCE(deleting,0)=0`, accountID, userID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return err
	}
	if version < 1 || current != version {
		return ErrAccountConfigConflict
	}
	for _, remote := range unique {
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_folder_discovery(account_id,remote_path) VALUES(?,?) ON CONFLICT(account_id,remote_path) DO NOTHING`, accountID, remote); err != nil {
			return err
		}
	}
	return tx.Commit()
}
