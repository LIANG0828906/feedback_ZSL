package level

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Store persists commits to local files. Each commit is written
// atomically (tmp file + rename) and serially under a mutex, so commit
// order always matches op sequence order and writes never interleave.
// HEAD is updated only after the commit file is durable; a process killed
// between the two leaves at most an orphan commit file that is ignored on
// the next load.
type Store struct {
	dir string
	mu  sync.Mutex
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) commitPath(seq int) string {
	return filepath.Join(s.dir, fmt.Sprintf("commit-%06d.json", seq))
}

func (s *Store) headPath() string {
	return filepath.Join(s.dir, "HEAD")
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) Append(c *Commit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.commitPath(c.OpSeq), data); err != nil {
		return err
	}
	return writeFileAtomic(s.headPath(), []byte(strconv.Itoa(c.OpSeq)))
}

// Load returns all durable commits in sequence order. A missing HEAD
// means no committed history. Any corrupt commit file is an error; the
// caller decides to invalidate the history.
func (s *Store) Load() ([]*Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	headData, err := os.ReadFile(s.headPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	head, err := strconv.Atoi(strings.TrimSpace(string(headData)))
	if err != nil {
		return nil, fmt.Errorf("corrupt HEAD: %w", err)
	}
	commits := make([]*Commit, 0, head+1)
	for seq := 0; seq <= head; seq++ {
		data, err := os.ReadFile(s.commitPath(seq))
		if err != nil {
			return nil, fmt.Errorf("load commit %d: %w", seq, err)
		}
		var c Commit
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("parse commit %d: %w", seq, err)
		}
		if c.OpSeq != seq {
			return nil, fmt.Errorf("commit sequence mismatch at %d", seq)
		}
		commits = append(commits, &c)
	}
	return commits, nil
}

func (s *Store) Truncate(afterSeq int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "commit-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "commit-"), ".json"))
		if err != nil {
			continue
		}
		if n > afterSeq {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
				return err
			}
		}
	}
	if afterSeq < 0 {
		err := os.Remove(s.headPath())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return writeFileAtomic(s.headPath(), []byte(strconv.Itoa(afterSeq)))
}

func (s *Store) Invalidate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "HEAD" || strings.HasPrefix(name, "commit-") {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
