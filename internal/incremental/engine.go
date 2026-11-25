package incremental

import (
	"sort"
	"sync"
)

type Change struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type Entry struct {
	Key   string
	Value any
	Deps  []string
}

type Record struct {
	Key                string   `json:"key"`
	Status             string   `json:"status"`
	InvalidationSource string   `json:"invalidation_source"`
	ChangePath         string   `json:"change_path,omitempty"`
	Dependencies       []string `json:"dependencies"`
}

type Engine struct {
	mu       sync.Mutex
	entries  map[string]Entry
	reverse  map[string]map[string]bool
	affected map[string]Change
	records  []Record
}

func NewEngine() *Engine {
	return &Engine{
		entries:  map[string]Entry{},
		reverse:  map[string]map[string]bool{},
		affected: map[string]Change{},
	}
}

func (e *Engine) Invalidate(change Change) []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	visited := map[string]bool{change.Source: true}
	queue := []string{change.Source}
	var invalidatedKeys []string
	for len(queue) > 0 {
		dep := queue[0]
		queue = queue[1:]
		for key := range e.reverse[dep] {
			if visited[key] {
				continue
			}
			visited[key] = true
			if _, already := e.affected[key]; !already {
				e.affected[key] = change
				invalidatedKeys = append(invalidatedKeys, key)
			}
			queue = append(queue, key)
		}
	}
	sort.Strings(invalidatedKeys)
	return invalidatedKeys
}

func (e *Engine) GetOrCompute(key string, deps []string, compute func() (any, error)) (any, error) {
	e.mu.Lock()
	existing, ok := e.entries[key]
	change, stale := e.affected[key]
	if ok && !stale {
		rec := Record{Key: key, Status: "reused", InvalidationSource: "none", Dependencies: append([]string(nil), existing.Deps...)}
		e.records = append(e.records, rec)
		e.mu.Unlock()
		return existing.Value, nil
	}
	e.mu.Unlock()

	value, err := compute()
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.entries[key]
	for _, dep := range old.Deps {
		if set := e.reverse[dep]; set != nil {
			delete(set, key)
		}
	}
	for _, dep := range deps {
		if e.reverse[dep] == nil {
			e.reverse[dep] = map[string]bool{}
		}
		e.reverse[dep][key] = true
	}
	e.entries[key] = Entry{Key: key, Value: value, Deps: append([]string(nil), deps...)}
	source := "fresh_computation"
	path := ""
	if stale {
		source = change.Source
		path = change.Path
		delete(e.affected, key)
	}
	e.records = append(e.records, Record{
		Key: key, Status: "recomputed", InvalidationSource: source,
		ChangePath: path, Dependencies: append([]string(nil), deps...),
	})
	return value, nil
}

func (e *Engine) Records() []Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Record(nil), e.records...)
}

func (e *Engine) ResetRecords() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = nil
}

func (e *Engine) Has(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.entries[key]
	return ok
}
