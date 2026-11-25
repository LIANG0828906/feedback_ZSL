package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"

	"qkernel/internal/errs"
	"qkernel/internal/imagex"
)

const AlgoVersion = "qkernel-algo-v1"

type StepType string

const (
	NoiseStep      StepType = "noise"
	BlurStep       StepType = "blur"
	BlockStep      StepType = "block"
	ChromaStep     StepType = "chroma_shift"
	OverexposeStep StepType = "overexpose"
)

type Region struct {
	All  bool         `json:"all"`
	Rect *imagex.Rect `json:"rect,omitempty"`
}

func WholeRegion() Region { return Region{All: true} }

func RectRegion(r imagex.Rect) Region { return Region{Rect: &r} }

func (r Region) Resolve(bounds imagex.Rect) (imagex.Rect, error) {
	if r.All {
		return bounds, nil
	}
	if r.Rect == nil {
		return imagex.Rect{}, errs.New(errs.EPara, "region requires all=true or rect")
	}
	rr := r.Rect.Canonical()
	if rr.Empty() {
		return imagex.Rect{}, errs.New(errs.EPara, "region rect is empty")
	}
	clipped := rr.Intersect(bounds)
	if clipped.Empty() {
		return imagex.Rect{}, errs.New(errs.EPara, "region rect %v does not intersect frame %v", rr, bounds)
	}
	return clipped, nil
}

type Step struct {
	Type     StepType `json:"type"`
	Strength float64  `json:"strength"`
	Region   Region   `json:"region"`
}

type Chain struct {
	Steps []Step `json:"steps"`
}

func (c Chain) Fingerprint() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func Validate(c Chain, width, height int) error {
	if len(c.Steps) == 0 {
		return &errs.Error{Code: errs.EPara, Message: "chain must contain at least one step", StepIndex: 0, Field: "steps", BlockX: -1, BlockY: -1}
	}
	bounds := imagex.R(0, 0, width, height)
	for i, step := range c.Steps {
		stepNo := i + 1
		switch step.Type {
		case NoiseStep, BlurStep, BlockStep, ChromaStep, OverexposeStep:
		default:
			return &errs.Error{Code: errs.EPara, Message: "unknown step type " + string(step.Type), StepIndex: stepNo, Field: "steps[" + itoa(i) + "].type", BlockX: -1, BlockY: -1}
		}
		if math.IsNaN(step.Strength) || step.Strength < 0 || step.Strength > 1 {
			return &errs.Error{Code: errs.EPara, Message: "strength must be in [0,1]", StepIndex: stepNo, Field: "steps[" + itoa(i) + "].strength", BlockX: -1, BlockY: -1}
		}
		if _, err := step.Region.Resolve(bounds); err != nil {
			return &errs.Error{Code: errs.EPara, Message: err.Error(), StepIndex: stepNo, Field: "steps[" + itoa(i) + "].region", BlockX: -1, BlockY: -1}
		}
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

func EffectiveDomain(step Step, index int, bounds imagex.Rect) imagex.Rect {
	r, err := step.Region.Resolve(bounds)
	if err != nil {
		return imagex.Rect{}
	}
	margin := DomainMargin(step)
	_ = index
	return r.Expand(margin, bounds)
}

func DomainMargin(step Step) int {
	switch step.Type {
	case BlurStep:
		return int(math.Ceil(step.Strength*4)) + 1
	case ChromaStep:
		return int(math.Round(step.Strength*10)) + 2
	case BlockStep:
		return 8
	case NoiseStep, OverexposeStep:
		return 0
	}
	return 0
}
