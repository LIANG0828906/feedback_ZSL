package spectralrender

import (
	"sync"
	"sync/atomic"
)

type ToneCurve string

const (
	ToneLinear   ToneCurve = "linear"
	ToneReinhard ToneCurve = "reinhard"
	ToneACES     ToneCurve = "aces"
)

func (c ToneCurve) Valid() bool {
	return c == ToneLinear || c == ToneReinhard || c == ToneACES
}

// RenderParams is a complete, versioned rendering parameter snapshot. Tiles
// and checkpoints bind every value to Version so a preview update can never be
// mixed into an exact frame.
type RenderParams struct {
	Version                 uint64            `json:"version"`
	SampleCount             int               `json:"sample_count"`
	Integration             IntegrationMethod `json:"integration"`
	Bounces                 int               `json:"bounces"`
	Exposure                float64           `json:"exposure"`
	ToneCurve               ToneCurve         `json:"tone_curve"`
	Gamma                   float64           `json:"gamma"`
	TileSize                int               `json:"tile_size"`
	MaxParallel             int               `json:"max_parallel"`
	MemoryBudgetBytes       int64             `json:"memory_budget_bytes"`
	IndirectWeight          float64           `json:"indirect_weight"`
	VertexBitsOverride      int               `json:"vertex_bits_override,omitempty"`
	CoefficientBitsOverride int               `json:"coefficient_bits_override,omitempty"`
}

func DefaultRenderParams() RenderParams {
	return RenderParams{
		Version:           1,
		SampleCount:       35,
		Integration:       IntegrationTrapezoid,
		Bounces:           2,
		Exposure:          1.0,
		ToneCurve:         ToneACES,
		Gamma:             2.2,
		TileSize:          16,
		MaxParallel:       4,
		MemoryBudgetBytes: 32 << 20,
		IndirectWeight:    0.12,
	}
}

func (p RenderParams) Validate() *Error {
	if p.SampleCount < 4 || p.SampleCount > 4097 {
		return paraError("sample_count", "must be in [4,4097]")
	}
	if !p.Integration.Valid() {
		return paraError("integration", "must be one of rectangle, trapezoid, simpson")
	}
	if p.Bounces < 0 || p.Bounces > 8 {
		return paraError("bounces", "must be in [0,8]")
	}
	if !isFinite(p.Exposure) || p.Exposure < 0.01 || p.Exposure > 16 {
		return paraError("exposure", "must be finite and in [0.01,16]")
	}
	if !p.ToneCurve.Valid() {
		return paraError("tone_curve", "must be one of linear, reinhard, aces")
	}
	if !isFinite(p.Gamma) || p.Gamma < 0.5 || p.Gamma > 3.5 {
		return paraError("gamma", "must be finite and in [0.5,3.5]")
	}
	if p.TileSize < 4 || p.TileSize > 256 {
		return paraError("tile_size", "must be in [4,256]")
	}
	if p.MaxParallel < 1 || p.MaxParallel > 64 {
		return paraError("max_parallel", "must be in [1,64]")
	}
	if p.MemoryBudgetBytes <= 0 {
		return paraError("memory_budget_bytes", "must be positive")
	}
	if !isFinite(p.IndirectWeight) || p.IndirectWeight < 0 || p.IndirectWeight > 0.95 {
		return paraError("indirect_weight", "must be finite and in [0,0.95]")
	}
	if p.VertexBitsOverride != 0 && (p.VertexBitsOverride < 1 || p.VertexBitsOverride > 24) {
		return paraError("vertex_bits_override", "must be 0 or in [1,24]")
	}
	if p.CoefficientBitsOverride != 0 && (p.CoefficientBitsOverride < 1 || p.CoefficientBitsOverride > 24) {
		return paraError("coefficient_bits_override", "must be 0 or in [1,24]")
	}
	return nil
}

type ParamStore struct {
	mu      sync.Mutex
	current atomic.Pointer[RenderParams]
}

func NewParamStore(initial RenderParams) (*ParamStore, *Error) {
	if err := initial.Validate(); err != nil {
		return nil, err
	}
	if initial.Version == 0 {
		initial.Version = 1
	}
	s := &ParamStore{}
	p := initial
	s.current.Store(&p)
	return s, nil
}

func (s *ParamStore) Snapshot() RenderParams {
	p := s.current.Load()
	if p == nil {
		return RenderParams{}
	}
	return *p
}

// Update publishes one immutable snapshot. The version is monotonically
// incremented so interactive previews can always identify which values were
// used for a frame.
func (s *ParamStore) Update(p RenderParams) (RenderParams, *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.current.Load()
	var next uint64 = 1
	if prev != nil {
		next = prev.Version + 1
	}
	p.Version = next
	if err := p.Validate(); err != nil {
		return RenderParams{}, err
	}
	s.current.Store(&p)
	return p, nil
}

// ExactSession captures a parameter snapshot once. All work performed by this
// session uses the locked values even if the ParamStore is updated meanwhile.
type ExactSession struct {
	params RenderParams
}

func BeginExact(store *ParamStore) *ExactSession {
	return &ExactSession{params: store.Snapshot()}
}

func (s *ExactSession) Params() RenderParams { return s.params }

// RequireSameVersion is used at any boundary where independently produced
// values are combined (for example linear pixels and a tone-map snapshot).
func RequireSameVersion(a, b uint64, path string) *Error {
	if a != b {
		return verError(a, b, path, "parameter versions must not be mixed")
	}
	return nil
}
