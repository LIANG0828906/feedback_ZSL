package ortho

import (
	"os"
	"path/filepath"
)

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }
func joinPath(elem ...string) string    { return filepath.Join(elem...) }
