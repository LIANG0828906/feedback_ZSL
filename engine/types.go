// Package engine performs deterministic fixed-point cycle settlement,
// dependency-aware cache invalidation and incremental/full comparison.
package engine

import (
	"context"

	"economycore/fixed"
	"economycore/model"
)

const transitionDenominator = fixed.RateScale * fixed.WorkerScale

// Options configures deterministic settlement.
type Options struct {
	AlgorithmVersion string
	// Fault is an optional deterministic window/cycle fault injection hook.
	// It is useful for testing window isolation and retry.
	Fault func(window int, cycle int64) error
}

// NodeSnapshot is the per-cycle node result.
type NodeSnapshot struct {
	NodeID          string           `json:"nodeId"`
	Built           bool             `json:"built"`
	Level           int              `json:"level"`
	Recipe          string           `json:"recipe"`
	Workers         int              `json:"workers"`
	RateFixed       int64            `json:"rateFixed"`
	Stocks          map[string]int64 `json:"stocks"`
	Produced        map[string]int64 `json:"produced"`
	Consumed        map[string]int64 `json:"consumed"`
	NetRateFixed    map[string]int64 `json:"netRateFixed"`
	Overflow        map[string]int64 `json:"overflow"`
	Stalled         bool             `json:"stalled"`
	StallStartCycle int64            `json:"stallStartCycle"`
}

// CycleSnapshot is one logical cycle.
type CycleSnapshot struct {
	Cycle          int64            `json:"cycle"`
	Window         int              `json:"window"`
	CycleInWindow  int              `json:"cycleInWindow"`
	GlobalStocks   map[string]int64 `json:"globalStocks"`
	GlobalTotal    int64            `json:"globalTotal"`
	WorkersIdle    int64            `json:"workersIdle"`
	Nodes          []NodeSnapshot   `json:"nodes"`
	CommitSequence []string         `json:"commitSequence"`
}

// ReuseStatus summarizes whether a node's cached cycle transitions were
// reused during the last run.
type ReuseStatus struct {
	NodeID           string   `json:"nodeId"`
	Reused           bool     `json:"reused"`
	ReusedCycles     int      `json:"reusedCycles"`
	RecomputedCycles int      `json:"recomputedCycles"`
	Reasons          []string `json:"reasons,omitempty"`
}

// RunResult contains the complete incremental/full comparable output.
type RunResult struct {
	Cycles           []CycleSnapshot    `json:"cycles"`
	OverflowTotals   map[string]int64   `json:"overflowTotals"`
	FinalState       model.RuntimeState `json:"finalState"`
	IntentOffset     int                `json:"intentOffset"`
	Complete         bool               `json:"complete"`
	Reuse            []ReuseStatus      `json:"reuse,omitempty"`
	ReusedCycles     int                `json:"reusedCycles"`
	RecomputedCycles int                `json:"recomputedCycles"`
}

// WindowResult is the atomically committed unit of a runner window.
type WindowResult struct {
	Window           int                `json:"window"`
	StartCycle       int64              `json:"startCycle"`
	EndCycle         int64              `json:"endCycle"`
	Cycles           []CycleSnapshot    `json:"cycles"`
	State            model.RuntimeState `json:"state"`
	IntentOffset     int                `json:"intentOffset"`
	ReusedCycles     int                `json:"reusedCycles"`
	RecomputedCycles int                `json:"recomputedCycles"`
}

// Invalidation describes one cache entry invalidated by an edit.
type Invalidation struct {
	NodeID  string   `json:"nodeId"`
	Reasons []string `json:"reasons"`
}

// EditReport is emitted by an explicit graph/cache edit.
type EditReport struct {
	ChangedNode  string            `json:"changedNode"`
	Change       string            `json:"change"`
	Invalidated  []Invalidation    `json:"invalidated"`
	Unaffected   []string          `json:"unaffected"`
	Statuses     []NodeCacheStatus `json:"statuses"`
	GraphUpdated bool              `json:"graphUpdated"`
}

// NodeCacheStatus is the explicit per-node invalidated/reused disposition.
type NodeCacheStatus struct {
	NodeID      string   `json:"nodeId"`
	Invalidated bool     `json:"invalidated"`
	Reused      bool     `json:"reused"`
	Reasons     []string `json:"reasons,omitempty"`
}

type cacheKey struct {
	NodeID string
	Cycle  int64
}

type nodeTransition struct {
	NodeID          string
	Cycle           int64
	Signature       string
	Built           bool
	Level           int
	Recipe          string
	Workers         int
	RateFixed       int64
	ProgressBefore  int64
	ProgressAfter   int64
	Batches         int64
	Consumed        map[string]int64
	Produced        map[string]int64
	Stalled         bool
	StallStartCycle int64
}

type cycleStats struct {
	Reused         int
	Recomputed     int
	NodeReused     map[string]bool
	NodeRecomputed map[string]bool
	Reasons        map[string][]string
}

// Engine is a deterministic economic scheduler.
type Engine struct {
	model         *model.Model
	graph         *model.Graph
	opts          Options
	state         model.RuntimeState
	cache         map[cacheKey]nodeTransition
	forcedInvalid map[string][]string
	lastReasons   map[string][]string
}

// CycleRunner is implemented by Engine for runner integration tests.
type CycleRunner interface {
	RunWindow(context.Context, int) (WindowResult, error)
}
