//go:build windows

package spatialpick

import (
	"fmt"
	"os"
	"time"
)

type fileLock struct {
	path string
}

func acquireFileLock(path string) (*fileLock, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return &fileLock{path: path}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 2*time.Minute {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for snapshot lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *fileLock) Release() {
	if l != nil {
		_ = os.Remove(l.path)
	}
}
