package profile

import (
	"math"

	"github.com/gameaudio/workbench/internal/acoustics"
	"github.com/gameaudio/workbench/internal/model"
)

// Build 按听者聚合全部声源结果，输出逐听者路径点的声学剖面。
func Build(project *model.Project, results []model.Result) []model.Profile {
	byListener := map[string][]model.Result{}
	windowByListener := map[string]int{}
	for _, l := range project.Listeners {
		windowByListener[l.ID] = l.WindowID
	}
	for _, r := range results {
		byListener[r.ListenerID] = append(byListener[r.ListenerID], r)
	}

	profiles := make([]model.Profile, 0, len(project.Listeners))
	for _, l := range project.Listeners {
		rs := byListener[l.ID]
		pf := model.Profile{
			ListenerID: l.ID,
			WindowID:   windowByListener[l.ID],
			CurveMean:  make([]float64, acoustics.SampleCount),
		}
		var splSum float64
		pf.MaxSPL = math.Inf(-1)
		for _, r := range rs {
			pf.TotalPairs++
			if r.Failed {
				pf.FailedPairs++
				continue
			}
			if r.Occluded {
				pf.OccludedPairs++
			}
			splSum += r.SPL
			if r.SPL > pf.MaxSPL {
				pf.MaxSPL = r.SPL
			}
			for k := range pf.CurveMean {
				pf.CurveMean[k] += r.Curve[k]
			}
		}
		valid := pf.TotalPairs - pf.FailedPairs
		if valid > 0 {
			pf.MeanSPL = splSum / float64(valid)
			for k := range pf.CurveMean {
				pf.CurveMean[k] /= float64(valid)
			}
		} else {
			pf.MaxSPL = 0
		}
		profiles = append(profiles, pf)
	}
	return profiles
}
