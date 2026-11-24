//go:build fyne || windows

package desktop

type mediaPlayer interface {
	Play(path string) error
	Pause() error
}
