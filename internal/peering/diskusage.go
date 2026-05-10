//go:build linux

package peering

import (
	"fmt"
	"syscall"
)

func diskUsedPercent(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	if st.Blocks == 0 {
		return 0, nil
	}
	used := st.Blocks - st.Bavail
	pct := (used * 100) / st.Blocks // bounded to [0, 100]
	return int(pct), nil            //nolint:gosec // pct ≤ 100 fits int on every supported arch
}
