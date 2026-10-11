package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/voxmail/voxmail/internal/folderpolicy"
)

type folderDirectories struct {
	paths  map[string]map[string]bool
	owners map[string]map[string]bool
}

// FolderBindingsForPreview exposes owner-scoped physical identity, including
// indexed historical aliases, without inferring equivalent provider folders.
func (s *Store) FolderBindingsForPreview(ctx context.Context, userID, accountID string) (map[string]string, error) {
	current, err := s.FolderInventory(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	directories, err := s.folderDirectories(ctx, accountID, current)
	if err != nil {
		return nil, err
	}
	bindings := map[string]string{}
	for local, owners := range directories.owners {
		if len(owners) != 1 {
			return nil, fmt.Errorf("%w: ambiguous folder preview identity", ErrInvalidAccountConfig)
		}
		for remote := range owners {
			bindings[local] = remote
		}
	}
	return bindings, nil
}

// PhysicalFoldersForRemote identifies cache directories belonging to this exact
// provider mailbox, including indexed history after a physical remap.
func (s *Store) PhysicalFoldersForRemote(ctx context.Context, userID, accountID, remote string) ([]string, error) {
	current, err := s.FolderInventory(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	directories, err := s.folderDirectories(ctx, accountID, current)
	if err != nil {
		return nil, err
	}
	fallback := remote
	for name, local := range current {
		if folderpolicy.RemoteKey(name) == folderpolicy.RemoteKey(remote) {
			fallback = local
			break
		}
	}
	return directories.forRemote(remote, fallback)
}

// Retained associations extend an explicit remote selection across its physical
// remaps. They never infer equivalence between different provider folders.
func (s *Store) folderDirectories(ctx context.Context, accountID string, current map[string]string) (*folderDirectories, error) {
	result := &folderDirectories{paths: map[string]map[string]bool{}, owners: map[string]map[string]bool{}}
	add := func(remote, local string) {
		key := folderpolicy.RemoteKey(remote)
		if result.paths[key] == nil {
			result.paths[key] = map[string]bool{}
		}
		if result.owners[local] == nil {
			result.owners[local] = map[string]bool{}
		}
		result.paths[key][local] = true
		result.owners[local][key] = true
	}
	for remote, local := range current {
		add(remote, local)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT b.remote_path,b.local_path FROM account_folder_bindings b WHERE b.account_id=? AND EXISTS(SELECT 1 FROM mail_messages m WHERE m.account_id=b.account_id AND m.folder=b.local_path)`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var remote, local string
		if err := rows.Scan(&remote, &local); err != nil {
			return nil, err
		}
		add(remote, local)
	}
	return result, rows.Err()
}

func (d *folderDirectories) forRemote(remote, fallback string) ([]string, error) {
	key := folderpolicy.RemoteKey(remote)
	selected := map[string]bool{fallback: true}
	for local := range d.paths[key] {
		selected[local] = true
	}
	result := make([]string, 0, len(selected))
	for local := range selected {
		for owner := range d.owners[local] {
			if owner != key {
				return nil, fmt.Errorf("%w: physical folder %q has ambiguous retained remote identity", ErrInvalidAccountConfig, local)
			}
		}
		result = append(result, local)
	}
	sort.Strings(result)
	return result, nil
}
