//go:build linux

package maintenance

import (
	"fmt"
	"golang.org/x/sys/unix"
)

type DiskUsage struct {
	TotalBytes uint64
	FreeBytes  uint64
}

func StatDisk(path string) (DiskUsage, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return DiskUsage{}, fmt.Errorf("stat filesystem %s: %w", path, err)
	}
	return DiskUsage{
		TotalBytes: uint64(stat.Blocks) * uint64(stat.Bsize),
		FreeBytes:  uint64(stat.Bavail) * uint64(stat.Bsize),
	}, nil
}
