package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

// validateFolderDestinationsTx prevents a logical group or mapping edit from
// sharing mbsync's physical state between different remote folders. Existing
// observed destinations remain reserved during a pending remap.
func validateFolderDestinationsTx(ctx context.Context, tx *sql.Tx, accountID string, proposed map[string]string) error {
	old := map[string]string{}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT folder_map FROM accounts WHERE id=?`, accountID).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &old); err != nil {
			return err
		}
	}
	type binding struct{ remote, local string }
	var occupied []binding
	for remote, local := range old {
		occupied = append(occupied, binding{remote, local})
	}
	rows, err := tx.QueryContext(ctx, `SELECT remote_path,local_path FROM remote_folders WHERE account_id=? UNION ALL SELECT remote_path,'' FROM account_folder_discovery WHERE account_id=? UNION ALL SELECT remote_path,local_path FROM account_folder_bindings WHERE account_id=?`, accountID, accountID, accountID)
	if err != nil {
		return err
	}
	// mbsync always has an Inbox destination, even before provider discovery.
	available := map[string]string{"INBOX": localFolderPath(proposed, "INBOX")}
	for remote, local := range proposed {
		available[folderpolicy.RemoteKey(remote)] = local
	}
	for rows.Next() {
		var remote, local string
		if err := rows.Scan(&remote, &local); err != nil {
			rows.Close()
			return err
		}
		if local != "" {
			occupied = append(occupied, binding{remote, local})
		}
		key := folderpolicy.RemoteKey(remote)
		if _, exists := available[key]; !exists {
			// The wildcard sync channel still downloads unmapped provider folders.
			available[key] = localFolderPath(nil, remote)
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return rowErr
	}
	for remote, local := range available {
		occupied = append(occupied, binding{remote, local})
	}
	for remote, local := range available {
		key, destination := folderpolicy.RemoteKey(remote), strings.ToLower(filepathCleanFolder(local))
		for _, existing := range occupied {
			if folderpolicy.RemoteKey(existing.remote) != key && strings.ToLower(filepathCleanFolder(existing.local)) == destination {
				return fmt.Errorf("%w: local folder %q is reserved for remote folder %q; choose a distinct local folder and combine duplicates as a logical group", ErrInvalidAccountConfig, local, existing.remote)
			}
		}
	}
	return nil
}
