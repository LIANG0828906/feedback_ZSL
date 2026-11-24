package verify

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"maskpipe/internal/model"

	_ "modernc.org/sqlite"
)

type Violation struct {
	ChunkX int         `json:"chunk_x,omitempty"`
	ChunkY int         `json:"chunk_y,omitempty"`
	Stage  model.Stage `json:"stage,omitempty"`
	Field  string      `json:"field"`
	Want   string      `json:"want,omitempty"`
	Got    string      `json:"got,omitempty"`
}

type StoredRow struct {
	ChunkX         int         `json:"chunk_x"`
	ChunkY         int         `json:"chunk_y"`
	Stage          model.Stage `json:"stage"`
	CanonicalOrder int         `json:"canonical_order"`
	ParamVersion   string      `json:"param_version"`
	AlgoVersion    string      `json:"algo_version"`
	Downsample     float64     `json:"downsample"`
	SummaryJSON    string      `json:"summary_json"`
	GeometryJSON   string      `json:"geometry_json"`
	Blob           []byte      `json:"-"`
	ContentHash    string      `json:"content_hash"`
}

type TruthCheck struct {
	OK             bool             `json:"ok"`
	PredictedCount int              `json:"predicted_count"`
	TruthCount     int              `json:"truth_count"`
	Items          []TruthDeviation `json:"items,omitempty"`
}

type TruthDeviation struct {
	RegionID      int     `json:"region_id"`
	CentroidError float64 `json:"centroid_error_px"`
	AreaRelative  float64 `json:"area_relative_error"`
	BBoxError     float64 `json:"bbox_error_px"`
}

type Report struct {
	OK                    bool           `json:"ok"`
	Tool                  string         `json:"tool"`
	Database              string         `json:"database"`
	SnapshotID            string         `json:"snapshot_id"`
	AlgorithmVersion      string         `json:"algorithm_version"`
	ParamVersion          string         `json:"param_version"`
	ReconstructedMaskHash string         `json:"reconstructed_mask_hash"`
	RegionCount           int            `json:"region_count"`
	Regions               []model.Region `json:"regions"`
	ExpectedStages        int            `json:"expected_stages"`
	StoredStages          int            `json:"stored_stages"`
	FirstViolation        *Violation     `json:"first_violation,omitempty"`
	TruthCheck            *TruthCheck    `json:"truth_check,omitempty"`
	Note                  string         `json:"note"`
}

type expectedSummary struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func Run(ctx context.Context, dbPath, snapshotID string, scene *model.SceneManifest) (*Report, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if snapshotID == "" {
		if err := db.QueryRowContext(ctx, `SELECT snapshot_id FROM snapshot_meta GROUP BY snapshot_id LIMIT 1`).Scan(&snapshotID); err != nil {
			return nil, model.NewError(model.CodeMeta, "snapshot_id", "no checkpoint snapshot found: %v", err)
		}
	}
	report := &Report{OK: true, Tool: "maskpipe-independent-verifier", Database: dbPath, SnapshotID: snapshotID}

	meta, err := readMeta(ctx, db, snapshotID)
	if err != nil {
		return nil, err
	}
	report.AlgorithmVersion = meta["algo_version"]
	report.ParamVersion = meta["param_version"]
	if scene != nil {
		if got := meta["truth_fingerprint"]; got != scene.TruthFingerprint {
			report.OK = false
			report.FirstViolation = &Violation{Field: "truth_fingerprint", Want: scene.TruthFingerprint, Got: got}
			return report, nil
		}
		if report.AlgorithmVersion != scene.Parameters.ParamVersion && false {
			_ = scene
		}
	}

	rows, err := readRows(ctx, db, snapshotID)
	if err != nil {
		return nil, err
	}
	report.StoredStages = len(rows)
	if scene != nil {
		report.ExpectedStages = scene.ChunkCount * 3
	}

	rowMap := map[string]StoredRow{}
	for _, row := range rows {
		rowMap[key(row.ChunkX, row.ChunkY, row.Stage)] = row
		if got := hash(row.Blob); got != row.ContentHash {
			return fail(report, &Violation{
				ChunkX: row.ChunkX, ChunkY: row.ChunkY, Stage: row.Stage,
				Field: "content_hash", Want: row.ContentHash, Got: got,
			}), nil
		}
		if row.AlgoVersion != report.AlgorithmVersion {
			return fail(report, &Violation{
				ChunkX: row.ChunkX, ChunkY: row.ChunkY, Stage: row.Stage,
				Field: "algo_version", Want: report.AlgorithmVersion, Got: row.AlgoVersion,
			}), nil
		}
		if row.ParamVersion != report.ParamVersion {
			return fail(report, &Violation{
				ChunkX: row.ChunkX, ChunkY: row.ChunkY, Stage: row.Stage,
				Field: "param_version", Want: report.ParamVersion, Got: row.ParamVersion,
			}), nil
		}
		var summary expectedSummary
		if err := json.Unmarshal([]byte(row.SummaryJSON), &summary); err != nil {
			return fail(report, &Violation{
				ChunkX: row.ChunkX, ChunkY: row.ChunkY, Stage: row.Stage,
				Field: "summary_json", Got: err.Error(),
			}), nil
		}
		if scene != nil && (summary.Width <= 0 || summary.Height <= 0) {
			return fail(report, &Violation{
				ChunkX: row.ChunkX, ChunkY: row.ChunkY, Stage: row.Stage,
				Field: "summary_json.width_height", Got: fmt.Sprintf("%dx%d", summary.Width, summary.Height),
			}), nil
		}
	}

	if scene == nil {
		report.Note = "no scene manifest supplied; integrity checks only"
		return report, nil
	}

	var params model.ParameterSet
	if raw := meta["params_json"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &params)
	}
	if params.MinRegionArea == 0 {
		params.MinRegionArea = scene.Parameters.MinRegionArea
	}

	mask := make([]byte, scene.Width*scene.Height)
	for cy := 0; cy < scene.Rows; cy++ {
		for cx := 0; cx < scene.Cols; cx++ {
			chunk, ok := rowMap[key(cx, cy, model.StageClean)]
			if !ok {
				return fail(report, &Violation{ChunkX: cx, ChunkY: cy, Stage: model.StageClean, Field: "stage_row", Want: "committed clean stage"}), nil
			}
			w := min(scene.Tile.Width, scene.Width-cx*scene.Tile.Width)
			h := min(scene.Tile.Height, scene.Height-cy*scene.Tile.Height)
			if len(chunk.Blob) != w*h {
				return fail(report, &Violation{
					ChunkX: cx, ChunkY: cy, Stage: model.StageClean, Field: "mask_blob.length",
					Want: fmt.Sprint(w * h), Got: fmt.Sprint(len(chunk.Blob)),
				}), nil
			}
			for y := 0; y < h; y++ {
				dst := (cy*scene.Tile.Height+y)*scene.Width + cx*scene.Tile.Width
				src := y * w
				copy(mask[dst:dst+w], chunk.Blob[src:src+w])
			}
			statsRow, ok := rowMap[key(cx, cy, model.StageStats)]
			if !ok {
				return fail(report, &Violation{ChunkX: cx, ChunkY: cy, Stage: model.StageStats, Field: "stage_row", Want: "committed stats stage"}), nil
			}
			var storedRegions []model.Region
			if err := json.Unmarshal([]byte(statsRow.GeometryJSON), &storedRegions); err != nil {
				return fail(report, &Violation{
					ChunkX: cx, ChunkY: cy, Stage: model.StageStats, Field: "geometry_json", Got: err.Error(),
				}), nil
			}
			recomputed := AnalyzeRegionsIndependent(chunk.Blob, w, h, params.MinRegionArea)
			if v := compareStoredRegions(storedRegions, recomputed, cx, cy); v != nil {
				return fail(report, v), nil
			}
		}
	}

	report.ReconstructedMaskHash = hash(mask)
	report.Regions = AnalyzeRegionsIndependent(mask, scene.Width, scene.Height, params.MinRegionArea)
	report.RegionCount = len(report.Regions)
	report.TruthCheck = compareTruth(report.Regions, scene.TruthRegions, scene.Declared.Quality)
	if !report.TruthCheck.OK {
		report.OK = false
	}
	return report, nil
}

func fail(report *Report, v *Violation) *Report {
	report.OK = false
	report.FirstViolation = v
	return report
}

func key(x, y int, stage model.Stage) string {
	return fmt.Sprintf("%d:%d:%s", x, y, stage)
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readMeta(ctx context.Context, db *sql.DB, id string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key,value FROM snapshot_meta WHERE snapshot_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func readRows(ctx context.Context, db *sql.DB, id string) ([]StoredRow, error) {
	rows, err := db.QueryContext(ctx, `
SELECT chunk_x,chunk_y,stage,canonical_order,param_version,algo_version,
       downsample,summary_json,geometry_json,blob,content_hash
FROM stages WHERE snapshot_id=? ORDER BY canonical_order ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredRow
	for rows.Next() {
		var r StoredRow
		var stage string
		if err := rows.Scan(&r.ChunkX, &r.ChunkY, &stage, &r.CanonicalOrder,
			&r.ParamVersion, &r.AlgoVersion, &r.Downsample, &r.SummaryJSON,
			&r.GeometryJSON, &r.Blob, &r.ContentHash); err != nil {
			return nil, err
		}
		r.Stage = model.Stage(stage)
		out = append(out, r)
	}
	return out, rows.Err()
}

func compareStoredRegions(stored, recomputed []model.Region, cx, cy int) *Violation {
	if len(stored) != len(recomputed) {
		return &Violation{ChunkX: cx, ChunkY: cy, Stage: model.StageStats, Field: "regions.length",
			Want: fmt.Sprint(len(recomputed)), Got: fmt.Sprint(len(stored))}
	}
	for i := range stored {
		if stored[i].Area != recomputed[i].Area {
			return &Violation{ChunkX: cx, ChunkY: cy, Stage: model.StageStats,
				Field: fmt.Sprintf("regions[%d].area", i), Want: fmt.Sprint(recomputed[i].Area), Got: fmt.Sprint(stored[i].Area)}
		}
		if absFloat(stored[i].Centroid.X-recomputed[i].Centroid.X) > 1e-6 {
			return &Violation{ChunkX: cx, ChunkY: cy, Stage: model.StageStats,
				Field: fmt.Sprintf("regions[%d].centroid.x", i), Want: fmt.Sprint(recomputed[i].Centroid.X), Got: fmt.Sprint(stored[i].Centroid.X)}
		}
		if absFloat(stored[i].Centroid.Y-recomputed[i].Centroid.Y) > 1e-6 {
			return &Violation{ChunkX: cx, ChunkY: cy, Stage: model.StageStats,
				Field: fmt.Sprintf("regions[%d].centroid.y", i), Want: fmt.Sprint(recomputed[i].Centroid.Y), Got: fmt.Sprint(stored[i].Centroid.Y)}
		}
		if stored[i].BBox != recomputed[i].BBox {
			return &Violation{ChunkX: cx, ChunkY: cy, Stage: model.StageStats,
				Field: fmt.Sprintf("regions[%d].bbox", i), Want: fmt.Sprintf("%+v", recomputed[i].BBox), Got: fmt.Sprintf("%+v", stored[i].BBox)}
		}
	}
	return nil
}

func compareTruth(pred, truth []model.Region, tol model.QualityTolerance) *TruthCheck {
	check := &TruthCheck{PredictedCount: len(pred), TruthCount: len(truth), OK: len(pred) == len(truth)}
	if len(pred) != len(truth) {
		return check
	}
	for i := range pred {
		d := TruthDeviation{
			RegionID:      i + 1,
			CentroidError: hypot(pred[i].Centroid.X-truth[i].Centroid.X, pred[i].Centroid.Y-truth[i].Centroid.Y),
			AreaRelative:  absFloat(float64(pred[i].Area-truth[i].Area)) / float64(max(1, truth[i].Area)),
			BBoxError: maxFloat(
				absFloat(float64(pred[i].BBox.MinX-truth[i].BBox.MinX)),
				absFloat(float64(pred[i].BBox.MaxX-truth[i].BBox.MaxX)),
				absFloat(float64(pred[i].BBox.MinY-truth[i].BBox.MinY)),
				absFloat(float64(pred[i].BBox.MaxY-truth[i].BBox.MaxY)),
			),
		}
		check.Items = append(check.Items, d)
		if d.CentroidError > tol.CentroidPixels || d.AreaRelative > tol.AreaRelative || d.BBoxError > tol.BBoxPixels {
			check.OK = false
		}
	}
	return check
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func maxFloat(values ...float64) float64 {
	out := values[0]
	for _, v := range values[1:] {
		if v > out {
			out = v
		}
	}
	return out
}
func hypot(x, y float64) float64 {
	return sqrtApprox(x*x + y*y)
}
func sqrtApprox(v float64) float64 {
	if v <= 0 {
		return 0
	}
	z := v
	for i := 0; i < 24; i++ {
		z -= (z*z - v) / (2 * z)
	}
	return z
}
