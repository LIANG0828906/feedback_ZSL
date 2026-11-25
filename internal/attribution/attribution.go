package attribution

import (
	"math"
	"sort"

	"qkernel/internal/errs"
	"qkernel/internal/metrics"
	"qkernel/internal/synth"
)

const DefaultDeclaredShare = 0.45

type Contribution struct {
	StepIndex int     `json:"step_index"`
	Noise     float64 `json:"noise_delta"`
	Edge      float64 `json:"edge_delta"`
	Chroma    float64 `json:"chroma_delta"`
	Share     float64 `json:"share"`
}

type BlockAttribution struct {
	BlockX        int            `json:"block_x"`
	BlockY        int            `json:"block_y"`
	Rect          [4]int         `json:"rect"`
	Status        string         `json:"status"`
	StepIndex     int            `json:"step_index,omitempty"`
	StepType      string         `json:"step_type,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Candidates    []int          `json:"candidates,omitempty"`
	Contributions []Contribution `json:"contributions"`
}

type Report struct {
	BlockSize     int                `json:"block_size"`
	DeclaredShare float64            `json:"declared_share"`
	Blocks        []BlockAttribution `json:"blocks"`
}

func BuildReport(states []*metrics.Report, stepTypes []string, declaredShare float64) (*Report, error) {
	if declaredShare <= 0 || declaredShare >= 1 {
		declaredShare = DefaultDeclaredShare
	}
	if len(states) < 2 {
		return nil, errs.New(errs.EPara, "attribution requires base and at least one step metric report")
	}
	blockCount := len(states[0].Blocks)
	rep := &Report{BlockSize: states[0].BlockSize, DeclaredShare: declaredShare}
	var firstAmbiguous error

	for bi := 0; bi < blockCount; bi++ {
		first := states[0].Blocks[bi]
		ba := BlockAttribution{BlockX: first.X, BlockY: first.Y, Rect: [4]int{first.Rect.MinX, first.Rect.MinY, first.Rect.MaxX, first.Rect.MaxY}}
		stepCount := len(states) - 1
		dN := make([]float64, stepCount)
		dE := make([]float64, stepCount)
		dC := make([]float64, stepCount)
		var sumN, sumE, sumC float64
		for si := 1; si < len(states); si++ {
			cur := states[si].Blocks[bi]
			prev := states[si-1].Blocks[bi]
			k := si - 1
			dN[k] = math.Abs(cur.NoiseVariance - prev.NoiseVariance)
			dE[k] = math.Abs(cur.EdgeClarity - prev.EdgeClarity)
			dcb := cur.ChromaCbMean - prev.ChromaCbMean
			dcr := cur.ChromaCrMean - prev.ChromaCrMean
			dC[k] = math.Hypot(dcb, dcr)
			sumN += dN[k]
			sumE += dE[k]
			sumC += dC[k]
		}
		active := 0
		if sumN > 1e-12 {
			active++
		}
		if sumE > 1e-10 {
			active++
		}
		if sumC > 1e-9 {
			active++
		}
		if active == 0 {
			ba.Status = "unchanged"
			ba.Reason = "no measurable metric change"
			rep.Blocks = append(rep.Blocks, ba)
			continue
		}

		shares := make([]float64, stepCount)
		for k := 0; k < stepCount; k++ {
			var v float64
			if sumN > 1e-12 {
				v += dN[k] / sumN
			}
			if sumE > 1e-10 {
				v += dE[k] / sumE
			}
			if sumC > 1e-9 {
				v += dC[k] / sumC
			}
			shares[k] = v / float64(active)
			stepIndex := k + 1
			ba.Contributions = append(ba.Contributions, Contribution{StepIndex: stepIndex, Noise: dN[k], Edge: dE[k], Chroma: dC[k], Share: shares[k]})
		}

		ranked := append([]Contribution(nil), ba.Contributions...)
		sort.SliceStable(ranked, func(i, j int) bool {
			if ranked[i].Share == ranked[j].Share {
				return ranked[i].StepIndex < ranked[j].StepIndex
			}
			return ranked[i].Share > ranked[j].Share
		})
		if ranked[0].Share <= declaredShare {
			ba.Status = "ambiguous"
			ba.Reason = "no step contribution exceeds declared share"
			for i := 0; i < len(ranked) && i < 3; i++ {
				if ranked[i].Share > 1e-6 {
					ba.Candidates = append(ba.Candidates, ranked[i].StepIndex)
				}
			}
			if len(ba.Candidates) == 0 {
				for _, c := range ranked {
					ba.Candidates = append(ba.Candidates, c.StepIndex)
				}
			}
			err := errs.WithBlock(errs.New(errs.EAttr, ba.Reason), ba.BlockX, ba.BlockY, ba.Candidates)
			if firstAmbiguous == nil {
				firstAmbiguous = err
			}
		} else {
			chosen := ranked[0].StepIndex
			for _, c := range ba.Contributions {
				if c.Share > declaredShare {
					chosen = c.StepIndex
					break
				}
			}
			ba.Status = "attributed"
			ba.StepIndex = chosen
			if chosen-1 < len(stepTypes) {
				ba.StepType = stepTypes[chosen-1]
			}
			ba.Reason = "first step above declared contribution share"
		}
		rep.Blocks = append(rep.Blocks, ba)
	}
	return rep, firstAmbiguous
}

func CheckTruth(rep *Report, truth *synth.GroundTruth) error {
	if truth == nil {
		return errs.New(errs.ESchema, "truth is required for attribution check")
	}
	byCoord := map[[2]int]BlockAttribution{}
	for _, b := range rep.Blocks {
		byCoord[[2]int{b.BlockX, b.BlockY}] = b
	}
	for _, im := range truth.Implants {
		got, ok := byCoord[[2]int{im.BlockX, im.BlockY}]
		if !ok {
			return errs.WithBlock(errs.New(errs.EAttr, "truth implant block missing from report"), im.BlockX, im.BlockY, []int{im.StepIndex})
		}
		if got.Status != "attributed" || got.StepIndex != im.StepIndex {
			candidates := got.Candidates
			if len(candidates) == 0 {
				for _, c := range got.Contributions {
					candidates = append(candidates, c.StepIndex)
				}
			}
			return errs.WithBlock(errs.New(errs.EAttr, "truth implant expects step %d but attribution is step %d (%s)", im.StepIndex, got.StepIndex, got.Status), im.BlockX, im.BlockY, candidates)
		}
	}
	return nil
}
