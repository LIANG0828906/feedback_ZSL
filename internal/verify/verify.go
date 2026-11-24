package verify

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"texlab/internal/apperr"
	"texlab/internal/model"
	"texlab/internal/plan"
	"texlab/internal/runrecord"
	"texlab/internal/truth"
)

type Options struct {
	PlanPath          string
	TruthPath         string
	CheckpointDir     string
	RecordPath        string
	ReportPath        string
	SourceImage       model.Image
	SourceFingerprint string
}

type PatchStat struct {
	EntryID    string    `json:"entry_id"`
	EntryIndex int       `json:"entry_index"`
	Patch      string    `json:"patch"`
	X          int       `json:"x"`
	Y          int       `json:"y"`
	Expected   [4]uint16 `json:"expected"`
	Observed   [4]uint16 `json:"observed"`
	MaxDelta   uint16    `json:"max_delta"`
}

type AlphaStat struct {
	EntryID  string    `json:"entry_id"`
	X        int       `json:"x"`
	Y        int       `json:"y"`
	Expected [4]uint16 `json:"expected"`
	Observed [4]uint16 `json:"observed"`
}

type SegmentSummary struct {
	Segment int    `json:"segment"`
	Hash    string `json:"hash"`
	Depth   int    `json:"depth"`
	Layout  string `json:"layout"`
}

type Report struct {
	Schema            string                   `json:"schema"`
	PlanFingerprint   string                   `json:"plan_fingerprint"`
	SourceFingerprint string                   `json:"source_fingerprint"`
	TruthFingerprint  string                   `json:"truth_fingerprint"`
	AlgorithmVersion  string                   `json:"algorithm_version"`
	Status            string                   `json:"status"`
	ComparedRecord    bool                     `json:"compared_record"`
	CommitSequence    []int                    `json:"commit_sequence"`
	EntrySummaries    []runrecord.EntrySummary `json:"entry_summaries"`
	FinalSegments     []SegmentSummary         `json:"final_segments"`
	FinalDigest       string                   `json:"final_digest"`
	PatchStats        []PatchStat              `json:"patch_stats"`
	AlphaStats        []AlphaStat              `json:"alpha_stats"`
	Error             *apperr.Error            `json:"error,omitempty"`
}

type diskImage struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Depth    int    `json:"depth"`
	Channels int    `json:"channels"`
	Layout   string `json:"layout"`
	Alpha    string `json:"alpha"`
	PixelZ   string `json:"pixels_zlib_b64"`
}

type diskStage struct {
	EntryID      string      `json:"entry_id"`
	EntryIndex   int         `json:"entry_index"`
	Entry        model.Entry `json:"entry"`
	Segment      int         `json:"segment"`
	Image        diskImage   `json:"image"`
	CacheKey     string      `json:"cache_key"`
	ContentHash  string      `json:"content_hash"`
	InputHash    string      `json:"input_hash"`
	RegionHash   string      `json:"region_hash"`
	ParamHash    string      `json:"param_hash"`
	CoverageHash string      `json:"coverage_hash"`
}

type diskCheckpoint struct {
	Schema            string        `json:"schema"`
	PlanFingerprint   string        `json:"plan_fingerprint"`
	SourceFingerprint string        `json:"source_fingerprint"`
	ParamVersion      int           `json:"param_version"`
	AlgorithmVersion  string        `json:"algorithm_version"`
	Segment           model.Segment `json:"segment"`
	Stages            []diskStage   `json:"stages"`
	Final             diskImage     `json:"final"`
	FinalHash         string        `json:"final_hash"`
	CommitSeq         int           `json:"commit_seq"`
}

type checkpointImage struct {
	model.Image
	Hash string
}

func Run(opts Options) (*Report, error) {
	if opts.PlanPath == "" || opts.TruthPath == "" || opts.CheckpointDir == "" || opts.RecordPath == "" {
		return nil, apperr.New(apperr.EPara, "plan, truth, checkpoint, and record paths are required")
	}
	p, err := plan.Load(opts.PlanPath, opts.SourceImage)
	if err != nil {
		return nil, err
	}
	t, err := truth.Load(opts.TruthPath)
	if err != nil {
		return nil, err
	}
	record, err := runrecord.Read(opts.RecordPath)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Schema: model.ReportSchema, PlanFingerprint: model.PlanFingerprint(p),
		SourceFingerprint: opts.SourceFingerprint, TruthFingerprint: t.Fingerprint,
		AlgorithmVersion: model.AlgorithmVersion, Status: "MATCH",
	}
	fail := func(ae *apperr.Error) (*Report, error) {
		report.Status = "DIFF"
		report.Error = ae
		if opts.ReportPath != "" {
			if werr := WriteReport(opts.ReportPath, report); werr != nil {
				return report, werr
			}
		}
		return report, ae
	}
	if t.PlanFingerprint != report.PlanFingerprint || t.SourceFingerprint != report.SourceFingerprint {
		return fail(&apperr.Error{Code: apperr.EDiff, Field: "truth_fingerprint", Message: "truth manifest belongs to a different plan/source"})
	}
	if report.SourceFingerprint != "" && record.SourceFingerprint != "" && report.SourceFingerprint != record.SourceFingerprint {
		return fail(&apperr.Error{Code: apperr.EDiff, Field: "source_fingerprint", Message: "source fingerprint mismatch"})
	}
	if record.PlanFingerprint != report.PlanFingerprint || record.ParamVersion != p.Revision ||
		record.AlgorithmVersion != model.AlgorithmVersion {
		return fail(&apperr.Error{Code: apperr.EDiff, Field: "record_version", Message: "run record plan/version mismatch"})
	}
	segments := model.BuildSegments(opts.SourceImage.Width, opts.SourceImage.Height, p.Segments)
	files, err := filepath.Glob(filepath.Join(opts.CheckpointDir, "segment-*.json"))
	if err != nil {
		return fail(&apperr.Error{Code: apperr.ECheckpoint, Field: "checkpoint", Message: err.Error()})
	}
	sort.Strings(files)
	if len(files) != len(segments) {
		return fail(&apperr.Error{Code: apperr.EDiff, Field: "checkpoint_count", Message: fmt.Sprintf("checkpoint has %d segments, want %d", len(files), len(segments))})
	}
	byEntry := make(map[string]map[int]*checkpointImage)
	finals := make([]*checkpointImage, len(segments))
	var commitSeq []int
	for i, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fail(&apperr.Error{Code: apperr.ECheckpoint, Field: "checkpoint", Message: err.Error()})
		}
		var dc diskCheckpoint
		if err := json.Unmarshal(raw, &dc); err != nil {
			return fail(&apperr.Error{Code: apperr.ECheckpoint, Field: "checkpoint", Message: err.Error()})
		}
		if dc.Schema != model.CheckpointSchema || dc.PlanFingerprint != report.PlanFingerprint ||
			dc.SourceFingerprint != record.SourceFingerprint || dc.ParamVersion != p.Revision ||
			dc.AlgorithmVersion != model.AlgorithmVersion || dc.CommitSeq != i || dc.Segment.Index != i {
			return fail(&apperr.Error{Code: apperr.EDiff, Segment: i, Field: "checkpoint_version", Message: "checkpoint identity/version mismatch"})
		}
		commitSeq = append(commitSeq, dc.CommitSeq)
		if len(dc.Stages) != len(p.Entries) {
			return fail(&apperr.Error{Code: apperr.EDiff, Segment: i, Field: "checkpoint_stages", Message: "stage count mismatch"})
		}
		for j, ds := range dc.Stages {
			if ds.EntryID != p.Entries[j].ID || ds.Segment != i {
				return fail(&apperr.Error{Code: apperr.EDiff, EntryIndex: j + 1, Segment: i, Field: "entry_identity", Message: "checkpoint entry order mismatch"})
			}
			im, err := decodeImage(ds.Image)
			if err != nil || im.ContentHash() != ds.ContentHash {
				return fail(&apperr.Error{Code: apperr.EDiff, EntryIndex: j + 1, Segment: i, Field: "content_hash", Message: "checkpoint stage content hash mismatch"})
			}
			m := byEntry[ds.EntryID]
			if m == nil {
				m = make(map[int]*checkpointImage)
				byEntry[ds.EntryID] = m
			}
			m[i] = &checkpointImage{Image: im, Hash: ds.ContentHash}
		}
		finalImage, err := decodeImage(dc.Final)
		if err != nil || finalImage.ContentHash() != dc.FinalHash {
			return fail(&apperr.Error{Code: apperr.EDiff, Segment: i, Field: "final_hash", Message: "checkpoint final hash mismatch"})
		}
		finals[i] = &checkpointImage{Image: finalImage, Hash: dc.FinalHash}
	}
	report.CommitSequence = commitSeq
	if !equalInts(record.CommitSequence, commitSeq) {
		return fail(&apperr.Error{Code: apperr.EDiff, Field: "commit_sequence", Message: "commit sequence differs from run record"})
	}
	// Verify per-entry intermediate summaries from persisted segments only.
	wantEntries := make(map[string]runrecord.EntrySummary)
	for _, es := range record.EntrySummaries {
		wantEntries[es.EntryID] = es
	}
	for _, e := range p.Entries {
		var stages []runrecord.StageDigest
		for seg := 0; seg < len(segments); seg++ {
			im := byEntry[e.ID][seg]
			if im == nil {
				return fail(&apperr.Error{Code: apperr.EDiff, EntryIndex: e.Index, Segment: seg, Field: "stage", Message: "missing intermediate segment"})
			}
			stages = append(stages, runrecord.StageDigest{
				EntryID: e.ID, EntryIndex: e.Index, Segment: seg, ContentHash: im.Hash,
			})
		}
		es := runrecord.EntrySummary{EntryID: e.ID, Index: e.Index, Digest: runrecord.EntryDigest(stages), Stages: stages}
		report.EntrySummaries = append(report.EntrySummaries, es)
		want, ok := wantEntries[e.ID]
		if !ok || want.Digest != es.Digest {
			return fail(&apperr.Error{Code: apperr.EDiff, EntryIndex: e.Index, Field: "entry_summary", Message: "intermediate summary differs from run record"})
		}
	}
	for i, fin := range finals {
		report.FinalSegments = append(report.FinalSegments, SegmentSummary{
			Segment: i, Hash: fin.Hash, Depth: fin.Image.Depth, Layout: fin.Image.Layout,
		})
	}
	report.FinalDigest = runrecord.FinalDigest(record.FinalSegments)
	if report.FinalDigest != record.FinalDigest || !sameFinalSegments(report.FinalSegments, record.FinalSegments) {
		return fail(&apperr.Error{Code: apperr.EDiff, Field: "final_digest", Message: "final image digest differs from run record"})
	}
	// Rebuild full per-entry images only from persisted segment pixels and
	// compare them to the independent truth manifest.
	for _, et := range t.Entries {
		im, err := assemble(byEntry[et.EntryID], segments, opts.SourceImage.Width, opts.SourceImage.Height)
		if err != nil {
			return fail(&apperr.Error{Code: apperr.EDiff, EntryIndex: et.EntryIndex, Field: "assemble", Message: err.Error()})
		}
		ref, err := truth.ImageFromEntry(et)
		if err != nil {
			return fail(&apperr.Error{Code: apperr.EDiff, EntryIndex: et.EntryIndex, Field: "truth_pixels", Message: err.Error()})
		}
		if d := model.FirstImageDiff(ref, im, model.Entry{ID: et.EntryID, Index: et.EntryIndex}, 0); d != nil {
			seg := segmentFor(segments, d.X, d.Y)
			ae := &apperr.Error{
				Code: apperr.EDiff, EntryIndex: et.EntryIndex, Segment: seg, Field: "pixels",
				Coordinate: &apperr.Point{X: d.X, Y: d.Y}, Expected: d.Expected, Actual: d.Actual,
				Message: fmt.Sprintf("first truth mismatch at (%d,%d) entry %d", d.X, d.Y, et.EntryIndex),
			}
			return fail(ae)
		}
		for _, patch := range et.Patches {
			observed := im.SemanticPixel(patch.X, patch.Y)
			var delta uint16
			for c := 0; c < 4; c++ {
				d := absDiff(patch.RGBA[c], observed[c])
				if d > delta {
					delta = d
				}
			}
			report.PatchStats = append(report.PatchStats, PatchStat{
				EntryID: et.EntryID, EntryIndex: et.EntryIndex, Patch: patch.Name,
				X: patch.X, Y: patch.Y, Expected: patch.RGBA, Observed: observed, MaxDelta: delta,
			})
			if observed != patch.RGBA {
				return fail(&apperr.Error{
					Code: apperr.EDiff, EntryIndex: et.EntryIndex, Segment: segmentFor(segments, patch.X, patch.Y),
					Field: "patch:" + patch.Name, Coordinate: &apperr.Point{X: patch.X, Y: patch.Y},
					Expected: patch.RGBA[:], Actual: observed[:],
					Message: fmt.Sprintf("patch %s differs at entry %d", patch.Name, et.EntryIndex),
				})
			}
		}
		for _, step := range et.AlphaSteps {
			observed := im.SemanticPixel(step.X, step.Y)
			report.AlphaStats = append(report.AlphaStats, AlphaStat{
				EntryID: et.EntryID, X: step.X, Y: step.Y, Expected: step.RGBA, Observed: observed,
			})
			if observed != step.RGBA {
				return fail(&apperr.Error{
					Code: apperr.EDiff, EntryIndex: et.EntryIndex, Segment: segmentFor(segments, step.X, step.Y),
					Field: "alpha_step", Coordinate: &apperr.Point{X: step.X, Y: step.Y},
					Expected: step.RGBA[:], Actual: observed[:],
					Message: fmt.Sprintf("alpha step differs at entry %d", et.EntryIndex),
				})
			}
		}
	}
	last := p.Entries[len(p.Entries)-1]
	lastFull, _ := assemble(byEntry[last.ID], segments, opts.SourceImage.Width, opts.SourceImage.Height)
	if lastFull.ContentHash() == "" || t.FinalHash == "" {
		return fail(&apperr.Error{Code: apperr.EDiff, Field: "final_truth", Message: "missing final truth hash"})
	}
	if t.FinalHash != finals[len(finals)-1].Hash {
		// The truth final hash is computed on a full image; persisted final is
		// segment-based. Compare pixel content instead of hashes here.
		finalTruth, err := truth.ImageFromEntry(t.Entries[len(t.Entries)-1])
		if err != nil {
			return fail(&apperr.Error{Code: apperr.EDiff, Field: "final_truth", Message: err.Error()})
		}
		if d := model.FirstImageDiff(finalTruth, lastFull, last, 0); d != nil {
			return fail(&apperr.Error{
				Code: apperr.EDiff, EntryIndex: last.Index, Segment: segmentFor(segments, d.X, d.Y),
				Field: "final_pixels", Coordinate: &apperr.Point{X: d.X, Y: d.Y},
				Expected: d.Expected, Actual: d.Actual, Message: "final truth pixel mismatch",
			})
		}
	}
	report.ComparedRecord = true
	if opts.ReportPath != "" {
		if err := WriteReport(opts.ReportPath, report); err != nil {
			return report, err
		}
	}
	return report, nil
}

func WriteReport(path string, report *Report) error {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "create report directory", Path: path, Details: err.Error()}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "write verification report", Path: path, Details: err.Error()}
	}
	if err := os.Rename(tmp, path); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "replace verification report", Path: path, Details: err.Error()}
	}
	return nil
}

func decodeImage(d diskImage) (model.Image, error) {
	zraw, err := base64.StdEncoding.DecodeString(d.PixelZ)
	if err != nil {
		return model.Image{}, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(zraw))
	if err != nil {
		return model.Image{}, err
	}
	raw, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return model.Image{}, err
	}
	if d.Width <= 0 || d.Height <= 0 || len(raw) != d.Width*d.Height*8 {
		return model.Image{}, fmt.Errorf("invalid persisted pixel buffer")
	}
	im := model.NewImage(d.Width, d.Height, d.Depth, d.Channels, d.Layout, d.Alpha)
	for i := range im.Pixels {
		im.Pixels[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
	}
	return im, nil
}

func assemble(stages map[int]*checkpointImage, segments []model.Segment, width, height int) (model.Image, error) {
	if len(segments) == 0 {
		return model.Image{}, fmt.Errorf("no segments")
	}
	first := stages[0]
	if first == nil {
		return model.Image{}, fmt.Errorf("segment 0 missing")
	}
	out := model.NewImage(width, height, first.Depth, first.Channels, first.Layout, first.Alpha)
	for _, seg := range segments {
		im := stages[seg.Index]
		if im == nil || im.Width != seg.W || im.Height != seg.H {
			return model.Image{}, fmt.Errorf("segment %d missing", seg.Index)
		}
		for y := 0; y < seg.H; y++ {
			copy(out.Pixels[((seg.Y+y)*width+seg.X)*4:((seg.Y+y)*width+seg.X+seg.W)*4], im.Pixels[y*seg.W*4:(y*seg.W+seg.W)*4])
		}
	}
	return out, nil
}

func segmentFor(segments []model.Segment, x, y int) int {
	for _, s := range segments {
		if x >= s.X && x < s.X+s.W && y >= s.Y && y < s.Y+s.H {
			return s.Index
		}
	}
	return 0
}

func sameFinalSegments(a []SegmentSummary, b []runrecord.FinalSegment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Segment != b[i].Segment || a[i].Hash != b[i].Hash ||
			a[i].Depth != b[i].Depth || a[i].Layout != b[i].Layout {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
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

func absDiff(a, b uint16) uint16 {
	if a > b {
		return a - b
	}
	return b - a
}
