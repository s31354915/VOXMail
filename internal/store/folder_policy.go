package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

// LogicalMailbox separates a presentation identity from its physical members
// and write destination. Group keys are never filesystem or IMAP paths.
type LogicalMailbox struct {
	Key           string   `json:"key"`
	Name          string   `json:"name"`
	PrimaryRemote string   `json:"primary_remote"`
	PrimaryLocal  string   `json:"primary_local"`
	Members       []string `json:"members"`
}

func (s *Store) resolveRolePolicy(ctx context.Context, account Account, role FolderRole) (FolderRole, error) {
	policy, err := s.FolderPolicy(ctx, account.UserID, account.ID)
	if err != nil {
		return FolderRole{}, err
	}
	key, excluded := policy.Membership(role.RemotePath)
	if excluded {
		return FolderRole{}, sql.ErrNoRows
	}
	if key != "" {
		remote, local, err := s.GroupDestination(ctx, account.UserID, account.ID, key)
		if err != nil {
			return FolderRole{}, err
		}
		role.RemotePath, role.LocalPath = remote, local
	} else {
		paths, err := s.remoteLocalPaths(ctx, account.UserID, account.ID)
		if err != nil {
			return FolderRole{}, err
		}
		for remote := range paths {
			if folderpolicy.RemoteKey(remote) == folderpolicy.RemoteKey(role.RemotePath) {
				role.LocalPath = localFolderPath(paths, role.RemotePath)
				break
			}
		}
	}
	return role, nil
}

func (s *Store) ListLogicalMailboxes(ctx context.Context, userID, accountID string) ([]LogicalMailbox, error) {
	policy, err := s.FolderPolicy(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	paths, err := s.remoteLocalPaths(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	physical, err := s.ListMailFolders(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	for _, local := range paths {
		physical = append(physical, local)
	}
	directories, err := s.folderDirectories(ctx, accountID, paths)
	if err != nil {
		return nil, err
	}
	hidden := map[string]bool{}
	for _, remote := range policy.Excluded {
		locals, err := directories.forRemote(remote, localFolderPath(paths, remote))
		if err != nil {
			return nil, err
		}
		for _, local := range locals {
			hidden[local] = true
		}
	}
	mailboxes := make([]LogicalMailbox, 0, len(physical)+len(policy.Groups))
	for _, group := range policy.Groups {
		members := make([]string, 0, len(group.Folders))
		for _, remote := range group.Folders {
			locals, err := directories.forRemote(remote, localFolderPath(paths, remote))
			if err != nil {
				return nil, err
			}
			for _, local := range locals {
				if !hidden[local] {
					members = append(members, local)
				}
				hidden[local] = true
			}
		}
		mailboxes = append(mailboxes, LogicalMailbox{Key: group.Key(), Name: group.Name, PrimaryRemote: group.Primary, PrimaryLocal: localFolderPath(paths, group.Primary), Members: members})
	}
	for _, local := range physical {
		if local == "" || hidden[local] {
			continue
		}
		hidden[local] = true
		remote := local
		for owner := range directories.owners[local] {
			remote = owner
		}
		if len(directories.owners[local]) > 1 {
			return nil, fmt.Errorf("%w: physical folder has ambiguous retained remote identity", ErrInvalidAccountConfig)
		}
		if local == "Inbox" && remote == local {
			remote = "INBOX"
		}
		current := localFolderPath(paths, remote)
		members, err := directories.forRemote(remote, current)
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			hidden[member] = true
		}
		mailboxes = append(mailboxes, LogicalMailbox{Key: current, Name: current, PrimaryRemote: remote, PrimaryLocal: current, Members: members})
	}
	sort.Slice(mailboxes, func(i, j int) bool {
		if mailboxes[i].Name == mailboxes[j].Name {
			return mailboxes[i].Key < mailboxes[j].Key
		}
		return mailboxes[i].Name < mailboxes[j].Name
	})
	return mailboxes, nil
}

func (s *Store) FolderPolicy(ctx context.Context, userID, accountID string) (folderpolicy.Policy, error) {
	policy, _, err := s.FolderPolicySnapshot(ctx, userID, accountID)
	return policy, err
}

// FolderPolicySnapshot reads the saved choices and version in one statement so
// an editor cannot combine old choices with a newer optimistic-write version.
func (s *Store) FolderPolicySnapshot(ctx context.Context, userID, accountID string) (folderpolicy.Policy, int, error) {
	var policy folderpolicy.Policy
	var raw string
	var version int
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(p.policy_json,'{}'),a.config_version
FROM accounts a LEFT JOIN account_folder_policies p ON p.account_id=a.id
WHERE a.id=? AND a.user_id=? AND COALESCE(a.deleting,0)=0`, accountID, userID).Scan(&raw, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, 0, ErrAccountNotFound
	}
	if err != nil {
		return policy, 0, err
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return policy, 0, fmt.Errorf("read folder policy: %w", err)
	}
	return policy, version, nil
}

func (s *Store) FolderInventory(ctx context.Context, userID, accountID string) (map[string]string, error) {
	return s.remoteLocalPaths(ctx, userID, accountID)
}

// RemoteFolderForLocal uses synchronized directory identity before a pending
// mapping edit. It never treats a logical group key as an IMAP mailbox name.
func (s *Store) RemoteFolderForLocal(ctx context.Context, userID, accountID, local string) (string, error) {
	if strings.HasPrefix(local, "/groups/") {
		return "", fmt.Errorf("%w: logical mailbox requires a physical destination", ErrInvalidAccountConfig)
	}
	paths, err := s.remoteLocalPaths(ctx, userID, accountID)
	if err != nil {
		return "", err
	}
	remote := ""
	claim := func(name string) error {
		if remote != "" && folderpolicy.RemoteKey(remote) != folderpolicy.RemoteKey(name) {
			return fmt.Errorf("%w: physical folder has ambiguous remote identity", ErrInvalidAccountConfig)
		}
		remote = name
		return nil
	}
	for name, path := range paths {
		if path != local {
			continue
		}
		if err := claim(name); err != nil {
			return "", err
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT b.remote_path FROM account_folder_bindings b JOIN accounts a ON a.id=b.account_id
WHERE b.account_id=? AND a.user_id=? AND COALESCE(a.deleting,0)=0 AND b.local_path=?`, accountID, userID, local)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		if err := claim(name); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if remote != "" {
		return remote, nil
	}
	if local == "Inbox" && len(paths) == 0 {
		return "INBOX", nil
	}
	return "", fmt.Errorf("%w: physical folder has no retained remote identity", ErrInvalidAccountConfig)
}

// PhysicalGroupFolders resolves logical membership to separate indexed Maildirs.
// The returned paths are query inputs only; returned messages keep their own
// folder/path/UID fields, and callers must not use a group key as a remote path.
func (s *Store) PhysicalGroupFolders(ctx context.Context, userID, accountID, key string) ([]string, error) {
	policy, err := s.FolderPolicy(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	group, ok := policy.GroupForKey(key)
	if !ok {
		return nil, fmt.Errorf("%w: unknown logical mailbox", ErrInvalidAccountConfig)
	}
	paths, err := s.remoteLocalPaths(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	directories, err := s.folderDirectories(ctx, accountID, paths)
	if err != nil {
		return nil, err
	}
	var folders []string
	seen := map[string]bool{}
	for _, remote := range group.Folders {
		locals, err := directories.forRemote(remote, localFolderPath(paths, remote))
		if err != nil {
			return nil, err
		}
		for _, local := range locals {
			if !seen[local] {
				seen[local] = true
				folders = append(folders, local)
			}
		}
	}
	return folders, nil
}

// GroupDestination resolves the explicitly chosen primary for write operations.
// A missing/stale logical identity is an error, never a guessed remote folder.
func (s *Store) GroupDestination(ctx context.Context, userID, accountID, key string) (remote, local string, err error) {
	policy, err := s.FolderPolicy(ctx, userID, accountID)
	if err != nil {
		return "", "", err
	}
	group, ok := policy.GroupForKey(key)
	if !ok {
		return "", "", fmt.Errorf("%w: unknown logical mailbox", ErrInvalidAccountConfig)
	}
	paths, err := s.remoteLocalPaths(ctx, userID, accountID)
	if err != nil {
		return "", "", err
	}
	return group.Primary, localFolderPath(paths, group.Primary), nil
}

func localFolderPath(paths map[string]string, remote string) string {
	if local := paths[remote]; local != "" {
		return local
	}
	if strings.EqualFold(remote, "INBOX") {
		for name, local := range paths {
			if strings.EqualFold(name, "INBOX") && local != "" {
				return local
			}
		}
		return "Inbox"
	}
	return remote
}

func (s *Store) remoteLocalPaths(ctx context.Context, userID, accountID string) (map[string]string, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT folder_map FROM accounts WHERE id=? AND user_id=? AND COALESCE(deleting,0)=0`, accountID, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	paths, err := validateAccountFolderMap(raw)
	if err != nil {
		return nil, err
	}
	discovery, err := s.DB.QueryContext(ctx, `SELECT remote_path FROM account_folder_discovery WHERE account_id=?`, accountID)
	if err != nil {
		return nil, err
	}
	for discovery.Next() {
		var remote string
		if err := discovery.Scan(&remote); err != nil {
			discovery.Close()
			return nil, err
		}
		known := false
		for existing := range paths {
			if folderpolicy.RemoteKey(existing) == folderpolicy.RemoteKey(remote) {
				known = true
				break
			}
		}
		if !known {
			paths[remote] = localFolderPath(paths, remote)
		}
	}
	rowErr := discovery.Err()
	discovery.Close()
	if rowErr != nil {
		return nil, rowErr
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT remote_path,local_path FROM remote_folders WHERE account_id=? ORDER BY updated_at,remote_path`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var remote, local string
		if err := rows.Scan(&remote, &local); err != nil {
			return nil, err
		}
		// Discovery records describe the actual synchronized directory until a
		// changed mapping has been successfully synchronized/reconciled.
		if local != "" {
			matched := false
			for existing := range paths {
				if folderpolicy.RemoteKey(existing) == folderpolicy.RemoteKey(remote) {
					paths[existing] = local
					matched = true
				}
			}
			if !matched {
				paths[remote] = local
			}
		}
	}
	return paths, rows.Err()
}

func folderSetPredicate(folders []string) (string, []any) {
	if len(folders) == 0 {
		return "0", nil
	}
	args := make([]any, len(folders))
	marks := make([]string, len(folders))
	for i, folder := range folders {
		args[i] = folder
		marks[i] = "?"
	}
	return "m.folder IN (" + strings.Join(marks, ",") + ")", args
}

func (s *Store) mailVisibilityPredicate(ctx context.Context, userID, accountID string) (string, []any, error) {
	query := `SELECT p.account_id,p.policy_json FROM account_folder_policies p JOIN accounts a ON a.id=p.account_id WHERE a.user_id=? AND COALESCE(a.deleting,0)=0`
	queryArgs := []any{userID}
	if accountID != "" {
		query += ` AND a.id=?`
		queryArgs = append(queryArgs, accountID)
	}
	rows, err := s.DB.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return "", nil, err
	}
	type selection struct {
		account string
		policy  folderpolicy.Policy
	}
	var selections []selection
	for rows.Next() {
		var item selection
		var raw string
		if err := rows.Scan(&item.account, &raw); err != nil {
			rows.Close()
			return "", nil, err
		}
		if err := json.Unmarshal([]byte(raw), &item.policy); err != nil {
			rows.Close()
			return "", nil, err
		}
		if len(item.policy.Excluded) > 0 {
			selections = append(selections, item)
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return "", nil, rowErr
	}
	var parts []string
	var args []any
	for _, item := range selections {
		paths, err := s.remoteLocalPaths(ctx, userID, item.account)
		if err != nil {
			return "", nil, err
		}
		directories, err := s.folderDirectories(ctx, item.account, paths)
		if err != nil {
			return "", nil, err
		}
		folders := make([]string, 0, len(item.policy.Excluded))
		for _, remote := range item.policy.Excluded {
			locals, err := directories.forRemote(remote, localFolderPath(paths, remote))
			if err != nil {
				return "", nil, err
			}
			folders = append(folders, locals...)
		}
		predicate, folderArgs := folderSetPredicate(folders)
		parts = append(parts, "NOT (m.account_id=? AND "+predicate+")")
		args = append(args, item.account)
		args = append(args, folderArgs...)
	}
	if len(parts) == 0 {
		return "1", nil, nil
	}
	return strings.Join(parts, " AND "), args, nil
}

// mailFolderPredicate also expands a physical role's folder when the owner
// explicitly placed it in a group, so unread Inbox queries use that same group.
func (s *Store) mailFolderPredicate(ctx context.Context, userID, accountID, folder string) (string, []any, error) {
	if accountID == "" {
		if strings.HasPrefix(folder, "/groups/") {
			return "", nil, ErrInvalidAccountConfig
		}
		predicate, args := folderSetPredicate([]string{folder})
		return predicate, args, nil
	}
	policy, err := s.FolderPolicy(ctx, userID, accountID)
	if errors.Is(err, ErrAccountNotFound) {
		return "0", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	key := folder
	if !strings.HasPrefix(folder, "/groups/") {
		paths, err := s.remoteLocalPaths(ctx, userID, accountID)
		if err != nil {
			return "", nil, err
		}
		directories, err := s.folderDirectories(ctx, accountID, paths)
		if err != nil {
			return "", nil, err
		}
		for _, remote := range policy.Excluded {
			locals, err := directories.forRemote(remote, localFolderPath(paths, remote))
			if err != nil {
				return "", nil, err
			}
			for _, local := range locals {
				if local == folder {
					return "0", nil, nil
				}
			}
		}
		for _, group := range policy.Groups {
			for _, remote := range group.Folders {
				locals, err := directories.forRemote(remote, localFolderPath(paths, remote))
				if err != nil {
					return "", nil, err
				}
				for _, local := range locals {
					if local == folder {
						key = group.Key()
					}
				}
			}
		}
		if key == folder {
			if len(directories.owners[folder]) > 1 {
				return "", nil, fmt.Errorf("%w: physical folder has ambiguous retained remote identity", ErrInvalidAccountConfig)
			}
			for remote := range directories.owners[folder] {
				locals, err := directories.forRemote(remote, localFolderPath(paths, remote))
				if err != nil {
					return "", nil, err
				}
				predicate, args := folderSetPredicate(locals)
				return predicate, args, nil
			}
			predicate, args := folderSetPredicate([]string{folder})
			return predicate, args, nil
		}
	}
	folders, err := s.PhysicalGroupFolders(ctx, userID, accountID, key)
	if err != nil {
		return "", nil, err
	}
	predicate, args := folderSetPredicate(folders)
	return predicate, args, nil
}

// SaveFolderPolicy persists owner choices with the account's normal optimistic
// version guard. It does not rewrite local paths, messages or remote UID state.
func (s *Store) SaveFolderPolicy(ctx context.Context, userID, accountID string, version int, policy folderpolicy.Policy) error {
	return s.saveFolderConfiguration(ctx, userID, accountID, version, policy, nil, nil)
}

// SaveFolderConfiguration commits mappings, roles and logical choices together.
// A nil mapping preserves the current physical mapping/role configuration.
func (s *Store) SaveFolderConfiguration(ctx context.Context, userID, accountID string, version int, policy folderpolicy.Policy, mapping, roles map[string]string) error {
	return s.saveFolderConfiguration(ctx, userID, accountID, version, policy, mapping, roles)
}

func (s *Store) saveFolderConfiguration(ctx context.Context, userID, accountID string, version int, policy folderpolicy.Policy, newMapping, roles map[string]string) error {
	if (newMapping == nil) != (roles == nil) {
		return fmt.Errorf("%w: folder mappings and roles must be provided together", ErrInvalidAccountConfig)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int
	var rawMapping string
	err = tx.QueryRowContext(ctx, `SELECT config_version,folder_map FROM accounts
WHERE id=? AND user_id=? AND COALESCE(deleting,0)=0`, accountID, userID).Scan(&current, &rawMapping)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAccountNotFound
	}
	if err != nil {
		return err
	}
	if version < 1 || version != current {
		return ErrAccountConfigConflict
	}
	mapping, err := validateAccountFolderMap(rawMapping)
	if err != nil {
		return err
	}
	available := make([]string, 0, len(mapping))
	for remote := range mapping {
		available = append(available, remote)
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
		available = append(available, remote)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return rowErr
	}
	if newMapping != nil {
		known := make(map[string]bool, len(available))
		for _, remote := range available {
			known[folderpolicy.RemoteKey(remote)] = true
		}
		for remote := range newMapping {
			if !known[folderpolicy.RemoteKey(remote)] {
				return fmt.Errorf("%w: mapped folder %q is not available in this account", ErrInvalidAccountConfig, remote)
			}
		}
		data, err := json.Marshal(newMapping)
		if err != nil {
			return err
		}
		if _, err := validateAccountFolderMap(string(data)); err != nil {
			return err
		}
		if err := validateFolderDestinationsTx(ctx, tx, accountID, newMapping); err != nil {
			return err
		}
		if roles == nil {
			return fmt.Errorf("%w: explicit folder roles required with mappings", ErrInvalidAccountConfig)
		}
		if err := s.saveFolderRolesTx(ctx, tx, userID, accountID, roles, newMapping); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET folder_map=? WHERE id=?`, string(data), accountID); err != nil {
			return err
		}
	}
	if err := policy.Validate(available); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAccountConfig, err)
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_folder_policies(account_id,policy_json)
VALUES(?,?) ON CONFLICT(account_id) DO UPDATE SET policy_json=excluded.policy_json`, accountID, string(data)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET config_version=config_version+1 WHERE id=?`, accountID); err != nil {
		return err
	}
	return tx.Commit()
}
