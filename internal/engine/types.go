package engine

import (
	"retargetlab/internal/fsm"
	"retargetlab/internal/geom"
	"retargetlab/internal/model"
	"retargetlab/internal/solver"
	"retargetlab/internal/store"
)

type Config struct {
	Source          model.Skeleton
	Clip            model.Clip
	Target          model.TargetVariant
	Mapping         model.Mapping
	Params          solver.Params
	Store           *store.Store
	WindowSize      int
	MaxWorkers      int
	QueuePerWorker  int
	BufferPerWorker int
	FrameFault      func(frame int) error
}

type Invalidation struct {
	Frame   int      `json:"frame"`
	Joints  []string `json:"joints"`
	Reasons []string `json:"reasons"`
	Phase   int      `json:"phase"`
}

type InvalidationReport struct {
	Entries   []Invalidation `json:"entries"`
	Frames    int            `json:"frames"`
	Joints    []string       `json:"joints"`
	CacheHits int            `json:"cacheHits"`
}

type RuleDependency struct {
	RuleIndex int      `json:"ruleIndex"`
	Source    string   `json:"source"`
	Targets   []string `json:"targets"`
	Joints    []string `json:"joints"`
	Frames    []int    `json:"frames"`
}

type ScaleDependency struct {
	Key    string `json:"key"`
	Joint  string `json:"joint,omitempty"`
	Frames []int  `json:"frames"`
}

type FallbackDependency struct {
	Joint          string              `json:"joint"`
	Policy         model.MissingPolicy `json:"policy"`
	SourceAncestor string              `json:"sourceAncestor,omitempty"`
	Joints         []string            `json:"joints"`
	Frames         []int               `json:"frames"`
}

type FrameDependency struct {
	Frame        int      `json:"frame"`
	MappingRules []int    `json:"mappingRules"`
	ScaleKeys    []string `json:"scaleKeys"`
	Joints       []string `json:"joints"`
}

type DependencyGraph struct {
	Rules     []RuleDependency     `json:"rules"`
	Fallbacks []FallbackDependency `json:"fallbacks"`
	Scales    []ScaleDependency    `json:"scales"`
	Frames    []FrameDependency    `json:"frames"`
}

type FrameView struct {
	Frame          int                     `json:"frame"`
	State          fsm.FrameState          `json:"state"`
	Invalidated    bool                    `json:"invalidated"`
	InvalidJoints  []string                `json:"invalidJoints,omitempty"`
	InvalidReasons []string                `json:"invalidReasons,omitempty"`
	Error          string                  `json:"error,omitempty"`
	CacheHit       bool                    `json:"cacheHit"`
	Root           *geom.Vec3              `json:"root,omitempty"`
	RootShift      *geom.Vec3              `json:"rootShift,omitempty"`
	Contacts       []string                `json:"contacts,omitempty"`
	Constraint     *solver.ConstraintStats `json:"constraint,omitempty"`
	MappingHash    string                  `json:"mappingHash,omitempty"`
	ScaleValues    map[string]float64      `json:"scaleValues,omitempty"`
}

type WindowView struct {
	StartFrame   int    `json:"startFrame"`
	EndFrame     int    `json:"endFrame"`
	MappingHash  string `json:"mappingHash"`
	ContentHash  string `json:"contentHash"`
	CommitSeq    int64  `json:"commitSeq"`
	ParamVersion int    `json:"paramVersion"`
}

type RunStats struct {
	FramesSolved int   `json:"framesSolved"`
	CacheHits    int   `json:"cacheHits"`
	CacheMisses  int   `json:"cacheMisses"`
	FailedFrames []int `json:"failedFrames"`
}

type StateSnapshot struct {
	TargetName       string               `json:"targetName"`
	Source           model.Skeleton       `json:"source"`
	Target           model.Skeleton       `json:"target"`
	Clip             model.Clip           `json:"clip"`
	Mapping          model.Mapping        `json:"mapping"`
	ScaleValues      map[string]float64   `json:"scaleValues"`
	ParamVersion     int                  `json:"paramVersion"`
	AlgorithmVersion string               `json:"algorithmVersion"`
	TotalFrames      int                  `json:"totalFrames"`
	CommittedFrames  int                  `json:"committedFrames"`
	PendingFrames    int                  `json:"pendingFrames"`
	SolvingFrames    int                  `json:"solvingFrames"`
	FailedFrames     int                  `json:"failedFrames"`
	Progress         float64              `json:"progress"`
	Frames           []FrameView          `json:"frames"`
	Windows          []WindowView         `json:"windows"`
	Dependencies     DependencyGraph      `json:"dependencies"`
	Invalidations    []Invalidation       `json:"invalidations"`
	CacheHits        int                  `json:"cacheHits"`
	CacheMisses      int                  `json:"cacheMisses"`
	LastRun          RunStats             `json:"lastRun"`
	Recovery         store.RecoveryReport `json:"recovery"`
	StorePath        string               `json:"storePath,omitempty"`
	Pool             poolStats            `json:"pool"`
}

type FrameDetail struct {
	Frame        FrameView           `json:"frame"`
	Result       *solver.FrameResult `json:"result,omitempty"`
	Dependency   FrameDependency     `json:"dependency"`
	Invalidation *Invalidation       `json:"invalidation,omitempty"`
}

type ComparisonReport struct {
	Equal          bool   `json:"equal"`
	FramesCompared int    `json:"framesCompared"`
	FirstFrame     int    `json:"firstFrame"`
	FrozenSkipped  []int  `json:"frozenSkipped,omitempty"`
	Joint          string `json:"joint,omitempty"`
	Message        string `json:"message,omitempty"`
	Left           any    `json:"left,omitempty"`
	Right          any    `json:"right,omitempty"`
}
