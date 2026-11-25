//go:build !unix

package checkpoint

func lockFile(path string) (func(), error) {
	return func() {}, nil
}
