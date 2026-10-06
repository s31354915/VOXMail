//go:build !linux

package maintenance

import "fmt"

type DiskUsage struct {
	TotalBytes uint64
	FreeBytes  uint64
}

func StatDisk(path string) (DiskUsage, error) {
	return DiskUsage{}, fmt.Errorf("filesystem capacity checks are unsupported on this platform")
}
