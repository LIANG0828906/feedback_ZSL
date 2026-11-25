package maskqc

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
)

type VersionAcceptance struct {
	Version        string `json:"version"`
	Loaded         bool   `json:"loaded"`
	StrokeCount    int    `json:"strokeCount"`
	AlphaExact     bool   `json:"alphaExact"`
	PixelExact     bool   `json:"pixelExact"`
	TopologyOK     bool   `json:"topologyOk"`
	DirectionOK    bool   `json:"directionOk"`
	ResidualDigest string `json:"residualDigest"`
	Error          string `json:"error,omitempty"`
}

type PathAcceptance struct {
	Name          string   `json:"name"`
	Sequence      []string `json:"sequence"`
	CommitCount   int      `json:"commitCount"`
	AlphaHash     string   `json:"alphaHash"`
	TopologyOK    bool     `json:"topologyOk"`
	DirectionOK   bool     `json:"directionOk"`
	TopologyHash  string   `json:"topologyHash"`
	DirectionHash string   `json:"directionHash"`
}

type ProjectAcceptance struct {
	ID              string              `json:"id"`
	Width           int                 `json:"width"`
	Height          int                 `json:"height"`
	Versions        []VersionAcceptance `json:"versions"`
	Paths           []PathAcceptance    `json:"paths"`
	ThreeMaskEqual  bool                `json:"threeMaskEqual"`
	SequenceEqual   bool                `json:"sequenceEqual"`
	ConclusionEqual bool                `json:"conclusionEqual"`
	ScheduledEqual  bool                `json:"scheduledEqual"`
	Scheduler       SchedulerMetrics    `json:"scheduler"`
	OK              bool                `json:"ok"`
}

type AcceptanceReport struct {
	FixedSeed int64               `json:"fixedSeed"`
	Root      string              `json:"root"`
	DBPath    string              `json:"dbPath"`
	Projects  []ProjectAcceptance `json:"projects"`
	OK        bool                `json:"ok"`
}

func VerifyRoot(root, dbPath string) (*AcceptanceReport, error) {
	if _, err := EnsureSampleRoot(root); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "project-v3.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	report := &AcceptanceReport{FixedSeed: FixedSeed, Root: root, DBPath: dbPath, OK: true}
	for _, v3Path := range paths {
		dir := filepath.Dir(v3Path)
		pa, err := verifyProjectDir(dir, dbPath)
		if err != nil {
			return nil, err
		}
		report.Projects = append(report.Projects, pa)
		if !pa.OK {
			report.OK = false
		}
	}
	return report, nil
}

func verifyProjectDir(dir, dbPath string) (ProjectAcceptance, error) {
	pa := ProjectAcceptance{OK: true}
	var v3 *Project
	for _, version := range []string{"v1", "v2", "v3"} {
		path := filepath.Join(dir, "project-"+version+".json")
		p, err := LoadProjectFile(path)
		va := VersionAcceptance{Version: version, ResidualDigest: p.safeResidual()}
		if err != nil {
			va.Error = err.Error()
			pa.Versions = append(pa.Versions, va)
			pa.OK = false
			continue
		}
		va.Loaded = true
		va.StrokeCount = len(p.Strokes)
		res, err := NewEngine(p).RunFullRecompute()
		if err != nil {
			va.Error = err.Error()
			pa.OK = false
		} else {
			va.AlphaExact = res.AlphaExact
			va.PixelExact = res.PixelExact
			va.TopologyOK = res.Topology.OK
			va.DirectionOK = res.Directions.OK
			if !va.AlphaExact || !va.TopologyOK || !va.DirectionOK {
				pa.OK = false
			}
		}
		pa.Versions = append(pa.Versions, va)
		if version == "v3" {
			v3 = p
			pa.ID = p.ID
			pa.Width = p.Width
			pa.Height = p.Height
		}
	}
	if v3 == nil {
		pa.OK = false
		return pa, nil
	}

	incStore, err := OpenCheckpoint(dbPath)
	if err != nil {
		return pa, err
	}
	_ = incStore.ResetProject(v3.ID)
	inc, err := NewEngine(v3).RunSequential(RunOptions{Path: "incremental", Store: incStore, Commit: true})
	if err != nil {
		_ = incStore.Close()
		return pa, err
	}
	incSeq, err := incStore.Sequence(v3.ID)
	_ = incStore.Close()
	if err != nil {
		return pa, err
	}

	breakPath := dbPath + ".breakpoint"
	prefixStore, err := OpenCheckpoint(breakPath)
	if err != nil {
		return pa, err
	}
	_ = prefixStore.ResetProject(v3.ID)
	breakAt := len(v3.Strokes) / 2
	if _, err := NewEngine(v3).RunPrefix(prefixStore, breakAt); err != nil {
		_ = prefixStore.Close()
		return pa, err
	}
	if err := prefixStore.Close(); err != nil {
		return pa, err
	}
	resumeStore, err := OpenCheckpoint(breakPath)
	if err != nil {
		return pa, err
	}
	resume, err := NewEngine(v3).Resume(resumeStore)
	if err != nil {
		_ = resumeStore.Close()
		return pa, err
	}
	resumeSeq, _ := resumeStore.Sequence(v3.ID)
	_ = resumeStore.Close()

	full, err := NewEngine(v3).RunFullRecompute()
	if err != nil {
		return pa, err
	}
	sched := NewScheduler(2, 256, 4)
	scheduled, schedErr := NewEngine(v3).RunScheduled(sched, "", "")
	pa.Scheduler = sched.Metrics
	if schedErr != nil {
		pa.OK = false
	} else {
		pa.ScheduledEqual = bytesEqual(scheduled.Mask.Alpha, full.Mask.Alpha)
		if !pa.ScheduledEqual {
			pa.OK = false
		}
	}

	pa.Paths = []PathAcceptance{
		summarizePath("incremental", inc, incSeq),
		summarizePath("resume", resume, resumeSeq),
		summarizePath("full", full, full.Sequence),
	}
	pa.ThreeMaskEqual = bytesEqual(inc.Mask.Alpha, resume.Mask.Alpha) && bytesEqual(inc.Mask.Alpha, full.Mask.Alpha)
	pa.SequenceEqual = equalStringSlices(incSeq, resumeSeq) && equalStringSlices(incSeq, full.Sequence)
	pa.ConclusionEqual = pa.Paths[0].TopologyHash == pa.Paths[1].TopologyHash &&
		pa.Paths[0].TopologyHash == pa.Paths[2].TopologyHash &&
		pa.Paths[0].DirectionHash == pa.Paths[1].DirectionHash &&
		pa.Paths[0].DirectionHash == pa.Paths[2].DirectionHash
	if !pa.ThreeMaskEqual || !pa.SequenceEqual || !pa.ConclusionEqual {
		pa.OK = false
	}
	return pa, nil
}

func (p *Project) safeResidual() string {
	if p.ConvertReport == nil {
		return sha256Sum(nil)
	}
	return p.ConvertReport.ResidualDigest()
}

func summarizePath(name string, r *RunResult, seq []string) PathAcceptance {
	topo, _ := json.Marshal(r.Topology)
	dir, _ := json.Marshal(r.Directions)
	return PathAcceptance{
		Name: name, Sequence: seq, CommitCount: r.CommitCount,
		AlphaHash: HashBytes(r.Mask.Alpha), TopologyOK: r.Topology.OK, DirectionOK: r.Directions.OK,
		TopologyHash: sha256Sum(topo), DirectionHash: sha256Sum(dir),
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (r *AcceptanceReport) MustJSON() []byte {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return []byte(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
	}
	return b
}
