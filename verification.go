package grid

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type SchemaTraceSummary struct {
	Fills          int  `json:"fills"`
	Losses         int  `json:"losses"`
	SemanticLosses int  `json:"semanticLosses"`
	Truncations    int  `json:"truncations"`
	RoundTripOK    bool `json:"roundTripOk"`
}

type TileSummary struct {
	Col              int     `json:"col"`
	Row              int     `json:"row"`
	PolyCoverage     float64 `json:"polyCoverage"`
	CoveragePermille int     `json:"coveragePermille"`
	ContentHash      string  `json:"contentHash"`
	CacheReused      bool    `json:"cacheReused,omitempty"`
}

type PathConsistency struct {
	ParallelEqualsFull    bool   `json:"parallelEqualsFull"`
	RestartEqualsFull     bool   `json:"restartEqualsFull"`
	IncrementalEqualsFull bool   `json:"incrementalEqualsFull"`
	AssembledHash         string `json:"assembledHash,omitempty"`
	FirstDifference       *Error `json:"firstDifference,omitempty"`
}

type VerificationReport struct {
	Seed                  int64                         `json:"seed"`
	OK                    bool                          `json:"ok"`
	TileCount             int                           `json:"tileCount"`
	PhysicsRequests       int                           `json:"physicsRequests"`
	CommittedSnapshots    int                           `json:"committedSnapshots"`
	CommitLog             []string                      `json:"commitLog"`
	Pool                  PoolStats                     `json:"pool"`
	SchemaTraces          map[string]SchemaTraceSummary `json:"schemaTraces"`
	Tiles                 []TileSummary                 `json:"tiles"`
	RestartInvalidated    []InvalidationItem            `json:"restartInvalidated"`
	RestartRecovered      int                           `json:"restartRecovered"`
	RestartExpected       int                           `json:"restartExpected"`
	ThreePaths            PathConsistency               `json:"threePaths"`
	IncrementalReused     int                           `json:"incrementalReused"`
	IncrementalRecomputed int                           `json:"incrementalRecomputed"`
	Invalidation          *InvalidationPlan             `json:"invalidation,omitempty"`
	Injections            map[string]*Error             `json:"injections"`
	CrossProcessHash      string                        `json:"crossProcessHash"`
}

// RunVerification executes the deterministic acceptance workflow in dir.
func RunVerification(dir string) (*VerificationReport, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	level := Generate(DefaultSeed)
	cam := DefaultCamera()
	versions := DefaultSemanticVersions()
	report := &VerificationReport{
		Seed: DefaultSeed, TileCount: cam.Cols() * cam.Rows(), PhysicsRequests: len(level.Requests), SchemaTraces: map[string]SchemaTraceSummary{},
		Injections: map[string]*Error{}, RestartExpected: cam.Cols() * cam.Rows(),
	}
	if err := writeJSON(filepath.Join(dir, "requests.json"), level.Requests); err != nil {
		return report, err
	}
	if err := writeJSON(filepath.Join(dir, "truth.json"), level.Truths); err != nil {
		return report, err
	}

	for _, v := range []int{1, 2, 3} {
		art, err := ExportVersion(level, v)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("schema_v%d.json", v)), art.Data, 0o644); err != nil {
			return nil, err
		}
		roundErr := VerifyExportRoundTrip(art)
		semanticLosses := 0
		for _, l := range art.Report.Losses {
			if l.SemanticLoss {
				semanticLosses++
			}
		}
		report.SchemaTraces[fmt.Sprint(v)] = SchemaTraceSummary{
			Fills: decodeFillCount(art.Data), Losses: len(art.Report.Losses),
			SemanticLosses: semanticLosses, Truncations: len(art.Report.Truncations),
			RoundTripOK: roundErr == nil,
		}
		if roundErr != nil {
			return report, roundErr
		}
	}

	pool := NewRayPool(PoolConfig{MaxParallelism: 4, RayBufferLimit: 4, RayDelay: 250 * time.Microsecond}, nil)
	parallelScene, err := ProjectSceneWithPool(level, cam, pool)
	if err != nil {
		return report, err
	}
	if err := parallelScene.ValidateCoverage(DefaultCoverageCeilingPermille); err != nil {
		return report, err
	}
	report.Pool = pool.Stats()

	fullScene, err := ProjectScene(level, cam)
	if err != nil {
		return report, err
	}
	if diff := CompareScenes(fullScene, parallelScene); diff != nil {
		return report, diff
	}
	report.ThreePaths.ParallelEqualsFull = true
	report.CrossProcessHash = fullScene.Tiles[0].ContentHash

	dbPath := filepath.Join(dir, "tiles.db")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + ".tmp")
	store, err := OpenSnapshot(dbPath)
	if err != nil {
		return report, err
	}
	prefix := 3
	for i := 0; i < prefix && i < len(fullScene.Tiles); i++ {
		t := fullScene.Tiles[i]
		if err := store.CommitTile(t, cam, versions, fullScene.HitStats); err != nil {
			_ = store.Close()
			return report, err
		}
		report.CommitLog = append(report.CommitLog, fmt.Sprintf("commit tile %s seq=%d", t.ID, t.Seq))
		report.CommittedSnapshots++
	}
	if err := store.Close(); err != nil {
		return report, err
	}

	restartStore, err := OpenSnapshot(dbPath)
	if err != nil {
		return report, err
	}
	cachedBefore, err := restartStore.LoadAll()
	if err != nil {
		_ = restartStore.Close()
		return report, err
	}
	report.RestartRecovered = len(cachedBefore)

	restartScene, err := ProjectScene(level, cam)
	if err != nil {
		return report, err
	}
	restartCache, err := RunWithCache(restartStore, restartScene, cam, versions, nil)
	if err != nil {
		_ = restartStore.Close()
		return report, err
	}
	for _, o := range restartCache.Outcomes {
		if !o.Reused {
			report.CommitLog = append(report.CommitLog, fmt.Sprintf("commit tile %s seq=%d", o.Tile, o.Seq))
			report.CommittedSnapshots++
		}
	}
	if diff := CompareScenes(fullScene, restartScene); diff != nil {
		report.ThreePaths.FirstDifference = diff
		_ = restartStore.Close()
		return report, diff
	}
	report.ThreePaths.RestartEqualsFull = true
	report.ThreePaths.AssembledHash = AssembledColorHash(restartScene)

	plan, err := EditObstacle(level, cam, &versions, "OB01", "restitution", func(o *Obstacle) {
		o.Restitution = 9000
	})
	if err != nil {
		_ = restartStore.Close()
		return report, err
	}
	report.Invalidation = plan
	for _, item := range plan.Items {
		report.RestartInvalidated = append(report.RestartInvalidated, item)
	}

	incrementalScene, err := ProjectScene(level, cam)
	if err != nil {
		_ = restartStore.Close()
		return report, err
	}
	forced := map[string]bool{}
	for _, id := range plan.Tiles {
		forced[id.String()] = true
	}
	incCache, err := RunWithCache(restartStore, incrementalScene, cam, versions, forced)
	if err != nil {
		_ = restartStore.Close()
		return report, err
	}
	report.IncrementalReused = incCache.Reused
	report.IncrementalRecomputed = incCache.Recomputed

	editedFull, err := ProjectScene(level, cam)
	if err != nil {
		_ = restartStore.Close()
		return report, err
	}
	if diff := CompareScenes(editedFull, incrementalScene); diff != nil {
		report.ThreePaths.FirstDifference = diff
		_ = restartStore.Close()
		return report, diff
	}
	if incCache.Reused == 0 {
		diff := &Error{Code: CodeDiff, Field: "cache.reuse", Message: "expected at least one unaffected tile to be reused"}
		report.ThreePaths.FirstDifference = diff
		_ = restartStore.Close()
		return report, diff
	}
	report.ThreePaths.IncrementalEqualsFull = true
	if err := restartStore.Close(); err != nil {
		return report, err
	}

	report.Tiles = make([]TileSummary, 0, len(incrementalScene.Tiles))
	reuseBySeq := map[int]bool{}
	for _, o := range incCache.Outcomes {
		reuseBySeq[o.Seq] = o.Reused
	}
	for _, t := range incrementalScene.Tiles {
		report.Tiles = append(report.Tiles, TileSummary{
			Col: t.ID.Col, Row: t.ID.Row, PolyCoverage: t.PolyCoverageRatio,
			CoveragePermille: t.CoveragePermille, ContentHash: t.ContentHash, CacheReused: reuseBySeq[t.Seq],
		})
	}

	_, decodeErr := InjectDecodeFault(Generate(DefaultSeed))
	report.Injections["decode"] = decodeErr
	report.Injections["duplicate"] = InjectDuplicateFault(Generate(DefaultSeed))
	report.Injections["order"] = InjectOrderFault(level, cam)
	_, normErr := InjectNormFault(level, cam)
	report.Injections["norm"] = normErr
	report.Injections["block"] = InjectBlockFault(cam)
	_, bounceErr := InjectBounceFault(Generate(DefaultSeed))
	report.Injections["bounce"] = bounceErr

	pool.Shutdown()
	report.Pool = pool.Stats()
	report.OK = report.ThreePaths.RestartEqualsFull && report.ThreePaths.IncrementalEqualsFull

	if err := writeJSON(filepath.Join(dir, "summary.json"), report); err != nil {
		return report, err
	}
	restartDoc := map[string]any{
		"seed": report.Seed, "committedSnapshots": report.CommittedSnapshots,
		"commitLog": report.CommitLog, "restartRecovered": report.RestartRecovered,
		"restartExpected": report.RestartExpected, "restartInvalidated": report.RestartInvalidated,
		"threePaths": report.ThreePaths, "invalidation": report.Invalidation,
	}
	if err := writeJSON(filepath.Join(dir, "restart.json"), restartDoc); err != nil {
		return report, err
	}
	return report, nil
}

func decodeFillCount(data []byte) int {
	_, rep, err := DecodeVersion(data)
	if err != nil {
		return 0
	}
	return len(rep.Fills)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
