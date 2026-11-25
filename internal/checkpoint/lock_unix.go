//go:build unix

package checkpoint

import (
	"os"
	"syscall"

	"obsproj/internal/model"
)

func lockFile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &model.KernelError{
			Code:    model.CodeIO,
			Message: "failed to open checkpoint lock file",
			Cause:   err.Error(),
		}
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, &model.KernelError{
			Code:    model.CodeIO,
			Message: "failed to lock checkpoint",
			Cause:   err.Error(),
		}
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
