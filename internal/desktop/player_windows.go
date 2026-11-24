//go:build windows

package desktop

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

type platformPlayer struct{}

var (
	winmm          = syscall.NewLazyDLL("winmm.dll")
	mciSendStringW = winmm.NewProc("mciSendStringW")
)

func newPlayer() mediaPlayer {
	return platformPlayer{}
}

func (platformPlayer) Play(path string) error {
	escaped := strings.ReplaceAll(path, `"`, `\"`)
	_ = mci(`close spatialworkbench`)
	if err := mci(fmt.Sprintf(`open "%s" type waveaudio alias spatialworkbench`, escaped)); err != nil {
		return err
	}
	return mci(`play spatialworkbench`)
}

func (platformPlayer) Pause() error {
	return mci(`pause spatialworkbench`)
}

func mci(command string) error {
	ptr, err := syscall.UTF16PtrFromString(command)
	if err != nil {
		return err
	}
	code, _, _ := mciSendStringW.Call(uintptr(unsafe.Pointer(ptr)), 0, 0, 0)
	if code != 0 {
		return fmt.Errorf("MCI command %q failed with code %d", command, code)
	}
	return nil
}
