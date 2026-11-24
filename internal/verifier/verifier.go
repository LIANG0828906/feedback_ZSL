package verifier

import (
	"fmt"
	"sort"

	"fixedsampler/internal/hashutil"
	"fixedsampler/internal/loader"
	"fixedsampler/internal/model"
)

type PersistedRow struct {
	Artifact      model.ChunkArtifact
	ConfigFP      string
	ParamVersion  string
	AlgoVersion   string
	ContentHashes []string
}

type PointDeviation struct {
	PointID     string `json:"point_id"`
	StageSeq    int    `json:"stage_seq"`
	Plane       string `json:"plane"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	ExpectedFX  int64  `json:"expected_fixed"`
	ActualFX    int64  `json:"actual_fixed"`
	DeviationFX int64  `json:"deviation_fixed"`
	ToleranceFX int64  `json:"tolerance_fixed"`
	Within      bool   `json:"within"`
}

type Result struct {
	OK             bool                `json:"ok"`
	ProjectID      string              `json:"project_id"`
	ConfigFP       string              `json:"config_fingerprint"`
	TruthFP        string              `json:"truth_fingerprint"`
	Deviations     []PointDeviation    `json:"deviations"`
	FirstViolation *model.DiffLocation `json:"first_violation,omitempty"`
	ComparedChunks int                 `json:"compared_chunks"`
}

type refPlane struct {
	name   string
	width  int
	height int
	bits   int
	subX   int
	subY   int
	cumX   int
	cumY   int
	values []int64
}

func Verify(p *model.Project, truth *model.TruthManifest, rows map[int]PersistedRow, expectedConfigFP string) (*Result, error) {
	truthFP := loader.TruthFingerprint(truth)
	if truth.Fingerprint != "" && truth.Fingerprint != truthFP {
		return nil, &model.PipelineError{Code: model.CodeDiff, Message: "truth manifest fingerprint mismatch"}
	}
	if p.TruthFingerprint != "" && p.TruthFingerprint != truthFP {
		return nil, &model.PipelineError{Code: model.CodeDiff, Message: "project truth_fingerprint does not match truth manifest"}
	}
	if expectedConfigFP == "" {
		expectedConfigFP = loader.ConfigFingerprint(p)
	}

	result := &Result{OK: true, ProjectID: p.ProjectID, ConfigFP: expectedConfigFP, TruthFP: truthFP, ComparedChunks: len(rows)}
	indices := sortedRowIndices(rows)
	if len(indices) == 0 {
		loc := &model.DiffLocation{Field: "checkpoint_rows", Expected: "persisted chunks", Actual: "0"}
		result.OK = false
		result.FirstViolation = loc
		return result, loc
	}

	for _, idx := range indices {
		row := rows[idx]
		if row.ParamVersion != model.ParamVersion || row.AlgoVersion != model.AlgoVersion {
			loc := diffFor(row.Artifact, 0, "version", model.ParamVersion+"/"+model.AlgoVersion, row.ParamVersion+"/"+row.AlgoVersion)
			return fail(result, loc), loc
		}
		if row.ConfigFP != expectedConfigFP {
			loc := diffFor(row.Artifact, 0, "config_fingerprint", expectedConfigFP, row.ConfigFP)
			return fail(result, loc), loc
		}
		for seq, level := range row.Artifact.Levels {
			want := hashLevel(level)
			if seq >= len(row.ContentHashes) || row.ContentHashes[seq] != want {
				got := ""
				if seq < len(row.ContentHashes) {
					got = row.ContentHashes[seq]
				}
				loc := diffFor(row.Artifact, level.Seq, "content_hash", want, got)
				return fail(result, loc), loc
			}
		}
	}

	persisted, err := assembleLevels(rows, p)
	if err != nil {
		return nil, err
	}
	ref := initialRefPlanes(p, persisted[0])
	refLevels := map[int]map[string]*refPlane{0: cloneRef(ref)}

	for seq := 1; seq <= len(p.Config); seq++ {
		stage := p.Config[seq-1]
		if err := referenceStage(stage, ref, p.ValueScale); err != nil {
			return nil, err
		}
		refLevels[seq] = cloneRef(ref)
		if loc := compareTiles(rows, seq, ref); loc != nil {
			return fail(result, loc), loc
		}
	}

	for _, point := range truth.KnownPoints {
		planes := refLevels[point.StageSeq]
		if planes == nil {
			planes = ref
		}
		actualPlane := planes[point.Plane]
		var actual int64
		var ok bool
		if actualPlane != nil && point.X >= 0 && point.Y >= 0 && point.X < actualPlane.width && point.Y < actualPlane.height {
			actual = actualPlane.values[point.Y*actualPlane.width+point.X]
			ok = true
		}
		if !ok && point.StageSeq > 0 {
			if assembled := persisted[point.StageSeq]; assembled != nil {
				actualPlane = assembled[point.Plane]
				if actualPlane != nil && point.X < actualPlane.width && point.Y < actualPlane.height {
					actual = actualPlane.values[point.Y*actualPlane.width+point.X]
					ok = true
				}
			}
		}
		if !ok {
			loc := locatePoint(rows, point.StageSeq, point.Plane, point.X, point.Y)
			loc.Field = "known_points." + point.ID + ".coordinate"
			loc.Expected = "in-bounds"
			loc.Actual = "out-of-bounds"
			return fail(result, loc), loc
		}
		dev := abs64(actual - point.ExpectedFX)
		pd := PointDeviation{
			PointID: point.ID, StageSeq: point.StageSeq, Plane: point.Plane, X: point.X, Y: point.Y,
			ExpectedFX: point.ExpectedFX, ActualFX: actual, DeviationFX: dev,
			ToleranceFX: point.ToleranceFX, Within: dev <= point.ToleranceFX,
		}
		result.Deviations = append(result.Deviations, pd)
		if !pd.Within {
			loc := locatePoint(rows, point.StageSeq, point.Plane, point.X, point.Y)
			loc.Field = "known_points." + point.ID + ".fixed_value"
			loc.Expected = fmt.Sprint(point.ExpectedFX)
			loc.Actual = fmt.Sprint(actual)
			return fail(result, loc), loc
		}
	}

	for _, boundary := range truth.BlockBoundaries {
		level := persisted[boundary.AfterSeq]
		if level == nil || level[boundary.Plane] == nil {
			loc := &model.DiffLocation{StageSeq: boundary.AfterSeq, Field: "block_boundaries." + boundary.ID + ".plane"}
			return fail(result, loc), loc
		}
		if loc := checkBoundary(rows, boundary, level[boundary.Plane]); loc != nil {
			return fail(result, loc), loc
		}
	}
	return result, nil
}

func (r *Result) String() string {
	if r == nil || r.FirstViolation == nil {
		return fmt.Sprintf("VERIFY_OK project=%s chunks=%d points=%d truth_fp=%s", r.ProjectID, r.ComparedChunks, len(r.Deviations), r.TruthFP)
	}
	return "VERIFY_FAIL " + r.FirstViolation.Error()
}

func fail(r *Result, loc *model.DiffLocation) *Result {
	r.OK = false
	r.FirstViolation = loc
	return r
}

func sortedRowIndices(rows map[int]PersistedRow) []int {
	out := make([]int, 0, len(rows))
	for idx := range rows {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

func diffFor(chunk model.ChunkArtifact, seq int, field string, expected, actual any) *model.DiffLocation {
	return &model.DiffLocation{
		ChunkIndex: chunk.ChunkIndex, ChunkX: chunk.ChunkX, ChunkY: chunk.ChunkY,
		StageSeq: seq, Field: field, Expected: fmt.Sprint(expected), Actual: fmt.Sprint(actual),
	}
}

func hashLevel(level model.LevelArtifact) string {
	hashes := make([]string, 0, len(level.Tiles))
	for _, tile := range level.Tiles {
		hashes = append(hashes, hashutil.CanonicalHash(tile))
	}
	return hashutil.CanonicalHash(hashes)
}
