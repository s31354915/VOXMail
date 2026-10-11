package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

// BindSyncFolders records the directory/remote associations used by the current
// generated sync configuration before mbsync can download any partial messages.
// It does not replace observations or advance the account configuration version.
func (s *Store) BindSyncFolders(ctx context.Context, userID, accountID string, version int) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT config_version,folder_map FROM accounts WHERE id=? AND user_id=? AND COALESCE(deleting,0)=0`, accountID, userID).Scan(&current, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return err
	}
	if version < 1 || current != version {
		return ErrAccountConfigConflict
	}
	mapping, err := validateAccountFolderMap(raw)
	if err != nil {
		return err
	}
	if err := validateFolderDestinationsTx(ctx, tx, accountID, mapping); err != nil {
		return err
	}
	known := map[string]string{"INBOX": "INBOX"}
	for remote := range mapping {
		known[folderpolicy.RemoteKey(remote)] = remote
	}
	rows, err := tx.QueryContext(ctx, `SELECT remote_path FROM remote_folders WHERE account_id=? UNION SELECT remote_path FROM account_folder_discovery WHERE account_id=?`, accountID, accountID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var remote string
		if err := rows.Scan(&remote); err != nil {
			rows.Close()
			return err
		}
		known[folderpolicy.RemoteKey(remote)] = remote
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return rowErr
	}
	for key, remote := range known {
		local := filepathCleanFolder(localFolderPath(mapping, remote))
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO account_folder_bindings(account_id,local_path,remote_path) VALUES(?,?,?)`, accountID, local, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}
