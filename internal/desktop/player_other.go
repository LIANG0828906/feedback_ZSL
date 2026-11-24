//go:build fyne && !windows

package desktop

import (
	"fmt"
	"os/exec"
	"sync"
)

type platformPlayer struct {
	mu      sync.Mutex
	process *exec.Cmd
}

func newPlayer() mediaPlayer {
	return &platformPlayer{}
}

func (p *platformPlayer) Play(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
	var command *exec.Cmd
	if _, err := exec.LookPath("ffplay"); err == nil {
		command = exec.Command("ffplay", "-nodisp", "-autoexit", "-loglevel", "error", path)
	} else if _, err := exec.LookPath("xdg-open"); err == nil {
		command = exec.Command("xdg-open", path)
	} else {
		return fmt.Errorf("no local audio player found; WAV is available at %s", path)
	}
	if err := command.Start(); err != nil {
		return err
	}
	p.process = command
	return nil
}

func (p *platformPlayer) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.process == nil {
		return nil
	}
	p.stopLocked()
	return nil
}

func (p *platformPlayer) stopLocked() {
	if p.process != nil && p.process.Process != nil {
		_ = p.process.Process.Kill()
		_ = p.process.Wait()
	}
	p.process = nil
}
