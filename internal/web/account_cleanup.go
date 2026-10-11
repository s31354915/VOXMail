package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) cleanupJobs(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, false)
	if !ok {
		return
	}
	jobs, err := s.Store.ListCleanupStatuses(r.Context(), u.ID, u.Role == "admin")
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) retryCleanupJob(w http.ResponseWriter, r *http.Request) {
	u, ok := s.require(w, r, true)
	if !ok {
		return
	}
	if err := s.Store.RetryCleanupJob(r.Context(), u.ID, r.PathValue("id"), u.Role == "admin"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "failed cleanup job not found")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
}

// RunAccountCleanup is a local application worker, including in offline mode.
// Application shutdown cancels and joins it before the database closes.
func (s *Server) RunAccountCleanup(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := s.cleanupAccountPass(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if s.log() != nil {
				s.log().Error("account cleanup pass failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) cleanupAccountPass(ctx context.Context) error {
	jobs, err := s.Store.ReadyCleanupJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		var failures []error
		for _, path := range job.Paths {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.removeOwnedPath(ctx, path); err != nil {
				failures = append(failures, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cleanupErr := errors.Join(failures...)
		if err := s.Store.FinishCleanupJob(ctx, job.ID, cleanupErr); err != nil {
			return err
		}
		if cleanupErr != nil && s.log() != nil {
			s.log().Warn("account cleanup incomplete; retrying later", "job_id", job.ID, "error", cleanupErr)
		}
	}
	return nil
}

func (s *Server) removeOwnedPath(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.DataRoot == "" {
		return errors.New("cleanup data root is not configured")
	}
	pathAbs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	dataRoot, err := filepath.Abs(filepath.Clean(s.DataRoot))
	if err != nil {
		return err
	}
	for _, subdir := range []string{"mail", "drafts", filepath.Join("config", "mbsync"), "quarantine"} {
		rel, err := filepath.Rel(filepath.Join(dataRoot, subdir), pathAbs)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		// Anchor both the configured tree and allowed subtree. Symlinks cannot
		// escape these handles, including when renamed during traversal.
		data, err := os.OpenRoot(dataRoot)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		root, err := data.OpenRoot(subdir)
		data.Close()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		err = removeCleanupTree(ctx, root, rel)
		root.Close()
		return err
	}
	return fmt.Errorf("path is outside an owned cleanup root: %q", path)
}

// Descend through anchored directory handles, never symlinks. Read bounded
// chunks and check cancellation between entries so shutdown can join promptly.
func removeCleanupTree(ctx context.Context, root *os.Root, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return root.Remove(name)
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer child.Close()
	opened, err := child.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) {
		return errors.New("cleanup directory changed during traversal")
	}
	reader, err := child.Open(".")
	if err != nil {
		return err
	}
	defer reader.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := reader.ReadDir(128)
		for _, entry := range entries {
			if err := removeCleanupTree(ctx, child, entry.Name()); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return root.Remove(name)
}
