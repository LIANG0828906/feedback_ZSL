package diag

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func httpClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second}
}

// copyCheckpointDir 把一个检查点目录完整复制到目标目录（用于恢复/重放独立快照）。
func copyCheckpointDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		in, err := os.Open(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(dst, e.Name()))
		if err != nil {
			in.Close()
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			return err
		}
		if err := in.Close(); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	return nil
}
