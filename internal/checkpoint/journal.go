package checkpoint

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"mirrorcast/internal/bizerror"
)

const journalFileName = "dispatch-journal.json"

type Journal struct {
	mu    sync.Mutex
	path  string
	Steps map[string]StepProgress `json:"steps"`
}

func LoadJournal(dir string) (*Journal, error) {
	j := &Journal{path: filepath.Join(dir, journalFileName), Steps: map[string]StepProgress{}}
	data, err := os.ReadFile(j.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return j, nil
		}
		return nil, bizerror.New(bizerror.ECheckpointTruncated, "cannot read dispatch journal: "+err.Error()).WithPath(j.path)
	}
	if err := json.Unmarshal(data, j); err != nil {
		return nil, bizerror.New(bizerror.ECheckpointTruncated, "dispatch journal is truncated or malformed: "+err.Error()).WithPath(j.path)
	}
	if j.Steps == nil {
		j.Steps = map[string]StepProgress{}
	}
	return j, nil
}

func (j *Journal) IsCompleted(key, step string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	progress, ok := j.Steps[key]
	if !ok {
		return false
	}
	for _, completed := range progress.Completed {
		if completed == step {
			return true
		}
	}
	return false
}

func (j *Journal) CompletedSteps(key string) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.Steps[key].Completed...)
}

func (j *Journal) MarkCompleted(tick int64, key, step string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	progress := j.Steps[key]
	progress.LogicalTick = tick
	for _, completed := range progress.Completed {
		if completed == step {
			return nil
		}
	}
	progress.Completed = append(progress.Completed, step)
	sort.Strings(progress.Completed)
	j.Steps[key] = progress
	return j.persistLocked()
}

func (j *Journal) ClearKey(key string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.Steps, key)
	return j.persistLocked()
}

func (j *Journal) persistLocked() error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return bizerror.New(bizerror.EInternal, "cannot encode dispatch journal: "+err.Error())
	}
	return writeAtomic(j.path, data)
}
