//go:build !windows

package kernel

import "os"

func atomicReplace(from, to string) error {
	return os.Rename(from, to)
}
