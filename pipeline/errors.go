package pipeline

import (
	"fmt"

	"texturekernel"
)

func errf(code texture.Code, path, field, format string, args ...any) *texture.KernelError {
	return &texture.KernelError{Code: code, Path: path, Field: field, Message: fmt.Sprintf(format, args...)}
}
