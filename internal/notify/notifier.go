package notify

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"configcenter/internal/model"
)

var ErrBroadcastPending = errors.New("coalesced broadcast is still pending")

type MergeItem struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version int64  `json:"version"`
}

type Broadcast struct {
	Namespace   string      `json:"namespace"`
	Version     int64       `json:"version"`
	RawChanges  int         `json:"raw_change_count"`
	Items       []MergeItem `json:"items"`
	WindowStart time.Time   `json:"window_start"`
	WindowEnd   time.Time   `json:"window_end"`
	BroadcastAt time.Time   `json:"broadcast_at"`
}

type namespaceState struct {
	dirty         bool
	pending       map[string]MergeItem
	rawChanges    int
	firstDirty    time.Time
	lastDirty     time.Time
	timer         *time.Timer
	lastBroadcast int64
	waitCh        chan struct{}
	recent        *Broadcast
}

type Notifier struct {
	publishMu sync.Mutex

	mu      sync.Mutex
	states  map[string]*namespaceState
	window  time.Duration
	maxWait time.Duration

	latest      func(context.Context, string) (int64, error)
	onBroadcast func(Broadcast)
	closed      bool
}

func New(
	window time.Duration,
	maxWait time.Duration,
	latest func(context.Context, string) (int64, error),
	onBroadcast func(Broadcast),
) *Notifier {
	return &Notifier{
		states:      make(map[string]*namespaceState),
		window:      window,
		maxWait:     maxWait,
		latest:      latest,
		onBroadcast: onBroadcast,
	}
}

func (n *Notifier) stateLocked(namespace string) *namespaceState {
	state, ok := n.states[namespace]
	if !ok {
		state = &namespaceState{
			pending: make(map[string]MergeItem),
			waitCh:  make(chan struct{}),
		}
		n.states[namespace] = state
	}
	return state
}

// Publish serializes the durable commit with broadcast bookkeeping. A flush
// cannot observe a committed version before Record has incorporated it.
func (n *Notifier) Publish(
	ctx context.Context,
	namespace string,
	changes []model.Change,
	commit func() (int64, error),
) (int64, error) {
	n.publishMu.Lock()
	defer n.publishMu.Unlock()

	version, err := commit()
	if err != nil {
		return 0, err
	}
	n.record(namespace, version, changes, time.Now().UTC())
	return version, nil
}

func (n *Notifier) record(namespace string, version int64, changes []model.Change, now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}

	state := n.stateLocked(namespace)
	if !state.dirty {
		state.firstDirty = now
	}
	state.dirty = true
	state.lastDirty = now
	for _, change := range changes {
		state.pending[change.Key] = MergeItem{
			Key:     change.Key,
			Value:   change.Value,
			Version: version,
		}
		state.rawChanges++
	}

	if state.timer == nil {
		state.timer = time.AfterFunc(n.window, func() {
			n.flush(namespace)
		})
		return
	}

	// Debounce short bursts, but do not let a continuous storm postpone the
	// broadcast forever.
	if n.maxWait <= 0 || now.Sub(state.firstDirty)+n.window <= n.maxWait {
		state.timer.Reset(n.window)
	}
}

func (n *Notifier) flush(namespace string) {
	n.publishMu.Lock()
	defer n.publishMu.Unlock()

	n.mu.Lock()
	state := n.stateLocked(namespace)
	if !state.dirty {
		state.timer = nil
		n.mu.Unlock()
		return
	}
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	windowStart := state.firstDirty
	windowEnd := state.lastDirty
	rawChanges := state.rawChanges
	pending := state.pending
	state.pending = make(map[string]MergeItem)
	n.mu.Unlock()

	latest, err := n.latest(context.Background(), namespace)
	if err != nil {
		n.mu.Lock()
		state := n.stateLocked(namespace)
		state.dirty = true
		for key, item := range pending {
			state.pending[key] = item
		}
		state.rawChanges = rawChanges
		if state.timer == nil {
			state.timer = time.AfterFunc(n.window, func() {
				n.flush(namespace)
			})
		}
		n.mu.Unlock()
		return
	}

	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	items := make([]MergeItem, 0, len(keys))
	for _, key := range keys {
		items = append(items, pending[key])
	}
	broadcastAt := time.Now().UTC()
	broadcast := Broadcast{
		Namespace:   namespace,
		Version:     latest,
		RawChanges:  rawChanges,
		Items:       items,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		BroadcastAt: broadcastAt,
	}

	// Publish the observability event before making the boundary visible to
	// waiters, so counters are already current when a client applies it.
	if n.onBroadcast != nil {
		n.onBroadcast(broadcast)
	}

	n.mu.Lock()
	state = n.stateLocked(namespace)
	state.dirty = false
	state.rawChanges = 0
	state.firstDirty = time.Time{}
	state.lastDirty = time.Time{}
	state.lastBroadcast = latest
	recent := broadcast
	state.recent = &recent
	close(state.waitCh)
	state.waitCh = make(chan struct{})
	n.mu.Unlock()
}

// Prime marks an already-durable baseline as available to clients. Startup
// seeding does not count as a merged broadcast, but clients starting at zero
// can still read the complete baseline through history.
func (n *Notifier) Prime(namespace string, version int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	state := n.stateLocked(namespace)
	if version > state.lastBroadcast {
		state.lastBroadcast = version
		close(state.waitCh)
		state.waitCh = make(chan struct{})
	}
}

// WaitForBroadcast only returns a version that has crossed a merge boundary.
// If a timeout lands inside a pending merge window, it extends once until the
// pending broadcast is complete instead of exposing an intermediate version.
func (n *Notifier) WaitForBroadcast(
	ctx context.Context,
	namespace string,
	knownVersion int64,
	minimumVersion int64,
	timeout time.Duration,
) (int64, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	extended := false

	for {
		n.mu.Lock()
		state := n.stateLocked(namespace)
		lastBroadcast := state.lastBroadcast
		waitCh := state.waitCh
		n.mu.Unlock()

		if lastBroadcast > knownVersion && lastBroadcast >= minimumVersion {
			return lastBroadcast, nil
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-waitCh:
		case <-deadline.C:
			latest, err := n.latest(context.Background(), namespace)
			if err != nil {
				return 0, err
			}
			if latest == knownVersion {
				return knownVersion, nil
			}
			if extended {
				return 0, ErrBroadcastPending
			}
			extended = true
			boundedWait := n.window + n.maxWait + 100*time.Millisecond
			if boundedWait < n.window+100*time.Millisecond {
				boundedWait = n.window + 100*time.Millisecond
			}
			deadline.Reset(boundedWait)
		}
	}
}

func (n *Notifier) Recent(namespace string) (Broadcast, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	state, ok := n.states[namespace]
	if !ok || state.recent == nil {
		return Broadcast{}, false
	}
	result := *state.recent
	result.Items = append([]MergeItem(nil), state.recent.Items...)
	return result, true
}

func (n *Notifier) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	for _, state := range n.states {
		if state.timer != nil {
			state.timer.Stop()
			state.timer = nil
		}
	}
}
