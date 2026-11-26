package web

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"

	"maskqc"
)

type dashboardData struct {
	Flash         string
	Status        string
	Corrupt       string
	Reset         string
	Project       *maskqc.Project
	Icon          *maskqc.Icon
	Icons         []*maskqc.Icon
	Result        *maskqc.IconResult
	Graph         *maskqc.DependencyGraph
	Commits       []maskqc.CommitRecord
	Budget        maskqc.BudgetSnapshot
	Invalidations []*maskqc.InvalidationReport
	Latest        *maskqc.InvalidationReport
	Three         threeSummary
	Versions      map[string]string
	TileViews     []tileView
}

type tileView struct {
	ID       string
	Affected bool
	Reusable bool
}

type threeSummary struct {
	OK          bool
	Error       string
	Events      int
	Domains     int
	MaskHash    string
	Diff        *maskqc.Error
	ReusedTiles int
	TotalTiles  int
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	iconID := s.selectIconID(r)
	s.mu.RLock()
	icon := s.project.Icon(iconID)
	result := s.results[iconID]
	graph := s.graphs[iconID]
	budget := s.engine.SchedulerSnapshot()
	invalidations := append([]*maskqc.InvalidationReport(nil), s.invalidations...)
	commits, err := s.store.ListCommits("")
	three := s.threeSummaryLocked()
	project := s.project
	icons := s.project.Icons
	s.mu.RUnlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Newest history first for the diagnostics panel.
	reversedCommits := make([]maskqc.CommitRecord, 0, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		reversedCommits = append(reversedCommits, commits[i])
	}
	if len(reversedCommits) > 40 {
		reversedCommits = reversedCommits[:40]
	}
	var latest *maskqc.InvalidationReport
	if len(invalidations) > 0 {
		latest = invalidations[len(invalidations)-1]
	}
	tileViews := []tileView{}
	if icon != nil {
		affected := map[string]bool{}
		reusable := map[string]bool{}
		if latest != nil {
			for _, id := range latest.AffectedTiles {
				affected[id] = true
			}
			for _, id := range latest.ReusableTiles {
				reusable[id] = true
			}
		}
		for _, id := range icon.TileIDs() {
			tileViews = append(tileViews, tileView{ID: id, Affected: affected[id], Reusable: reusable[id]})
		}
	}

	data := dashboardData{
		Flash: r.URL.Query().Get("flash"), Status: r.URL.Query().Get("status"),
		Corrupt: r.URL.Query().Get("corrupt"), Reset: r.URL.Query().Get("reset"),
		Project: project, Icon: icon, Icons: icons, Result: result, Graph: graph,
		Commits: reversedCommits, Budget: budget, Invalidations: invalidations, Latest: latest,
		Three: three, TileViews: tileViews,
		Versions: map[string]string{
			"order": maskqc.OrderRulesVersion, "conversion": maskqc.ConversionParamsVersion,
			"algorithm": maskqc.AlgorithmVersion, "cache": maskqc.CacheVersion,
		},
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) threeSummaryLocked() threeSummary {
	sum := threeSummary{}
	if s.threeErr != nil {
		sum.Error = s.threeErr.Error()
		if de, ok := maskqc.AsError(s.threeErr); ok {
			sum.Diff = de
		}
		return sum
	}
	if s.threeReport == nil {
		sum.Error = "three-path report was not generated"
		return sum
	}
	full := s.threeReport.Full.Icons[maskqc.HarnessIconID]
	inc := s.threeReport.Incremental.Icons[maskqc.HarnessIconID]
	sum.OK = s.threeReport.Diff == nil && s.threeErr == nil
	sum.Events = len(s.threeReport.Full.EventSequence)
	if full != nil {
		sum.Domains = len(full.Analysis.Domains)
		sum.MaskHash = full.Accumulated.Hash()
		sum.TotalTiles = len(full.Tiles)
	}
	if inc != nil {
		for _, tile := range inc.Tiles {
			if tile.Reused {
				sum.ReusedTiles++
			}
		}
	}
	sum.Diff = s.threeReport.Diff
	return sum
}

func (s *Server) handlePNG(w http.ResponseWriter, r *http.Request) {
	iconID := s.selectIconID(r)
	kind := r.URL.Query().Get("kind")
	s.mu.RLock()
	icon := s.project.Icon(iconID)
	result := s.results[iconID]
	var m *maskqc.Mask
	var labels []int32
	switch kind {
	case "candidate":
		m = icon.Candidate
	case "truth":
		m = icon.Truth
	case "domains":
		if result != nil && result.Analysis != nil {
			labels = result.Analysis.Labels
		}
		m = result.Accumulated
	default:
		kind = "accum"
		if result != nil {
			m = result.Accumulated
		}
	}
	s.mu.RUnlock()
	if icon == nil || m == nil {
		http.Error(w, "mask not ready", http.StatusNotFound)
		return
	}

	scale := 8
	if z := r.URL.Query().Get("scale"); z != "" {
		if v := atoiOr(z, 8); v >= 1 && v <= 16 {
			scale = v
		}
	}
	img := renderMask(m, labels, scale)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func renderMask(m *maskqc.Mask, labels []int32, scale int) image.Image {
	r := image.Rect(0, 0, m.Width*scale, m.Height*scale)
	if labels != nil {
		img := image.NewRGBA(r)
		for y := 0; y < m.Height; y++ {
			for x := 0; x < m.Width; x++ {
				idx := y*m.Width + x
				c := color.RGBA{R: 245, G: 247, B: 250, A: 255}
				if labels[idx] > 0 {
					c = domainColor(int(labels[idx]))
				} else if m.Alpha[idx] > 0 {
					a := m.Alpha[idx]
					c = color.RGBA{R: 255 - a/3, G: 120 + a/4, B: 80, A: 255}
				}
				for sy := 0; sy < scale; sy++ {
					for sx := 0; sx < scale; sx++ {
						img.SetRGBA(x*scale+sx, y*scale+sy, c)
					}
				}
			}
		}
		return img
	}
	img := image.NewGray(r)
	for y := 0; y < m.Height; y++ {
		for x := 0; x < m.Width; x++ {
			v := m.Alpha[y*m.Width+x]
			c := color.Gray{Y: 255 - v}
			for sy := 0; sy < scale; sy++ {
				for sx := 0; sx < scale; sx++ {
					img.SetGray(x*scale+sx, y*scale+sy, c)
				}
			}
		}
	}
	return img
}

func domainColor(label int) color.RGBA {
	// Deterministic evenly-spaced hues converted with a compact 6-sector formula.
	h := float64((label*47)%360) / 60
	sector := int(h) % 6
	f := h - float64(int(h))
	v, p, q, t := uint8(230), uint8(40), uint8(40+int(150*(1-f))), uint8(40+int(150*f))
	switch sector {
	case 0:
		return color.RGBA{R: v, G: t, B: p, A: 255}
	case 1:
		return color.RGBA{R: q, G: v, B: p, A: 255}
	case 2:
		return color.RGBA{R: p, G: v, B: t, A: 255}
	case 3:
		return color.RGBA{R: p, G: q, B: v, A: 255}
	case 4:
		return color.RGBA{R: t, G: p, B: v, A: 255}
	default:
		return color.RGBA{R: v, G: p, B: q, A: 255}
	}
}

func atoiOr(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
