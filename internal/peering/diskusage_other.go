//go:build !linux

package peering

import "errors"

func diskUsedPercent(_ string) (int, error) {
	return 0, errors.New("disk usage check not supported on this platform")
}
