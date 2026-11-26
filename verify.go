package vpf

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
)

type SQLiteVerificationReport struct {
	Available     bool               `json:"available"`
	Driver        string             `json:"driver"`
	ThreePaths    *ThreePathReport   `json:"three_paths,omitempty"`
	Incremental   *IncrementalReport `json:"incremental,omitempty"`
	CheckpointDSN string             `json:"checkpoint_dsn,omitempty"`
}

type LocalVerificationReport struct {
	Seed           int64                         `json:"seed"`
	SchemaVersions []int                         `json:"schema_versions"`
	TraceCounts    map[int]int                   `json:"trace_counts"`
	TreeEquivalent bool                          `json:"tree_equivalent"`
	ActiveLayers   []string                      `json:"active_layers"`
	TruthVerified  bool                          `json:"truth_verified"`
	TruthSamples   int                           `json:"truth_samples"`
	LossyV1        *LossReport                   `json:"lossy_v1,omitempty"`
	Exceptions     map[string]*Error             `json:"exception_codes"`
	ThreePaths     *ThreePathReport              `json:"three_paths"`
	Incremental    map[string]*IncrementalReport `json:"incremental"`
	SQLite         *SQLiteVerificationReport     `json:"sqlite,omitempty"`
}

type SQLiteOpener func() (*sql.DB, error)

type SQLiteConfig struct {
	Driver            string
	ThreePathOpener   SQLiteOpener
	IncrementalOpener SQLiteOpener
	CheckpointDSN     string
}

func RunLocalVerification(ctx context.Context, sqliteConfig *SQLiteConfig) (*LocalVerificationReport, error) {
	bundle, err := GenerateSample(DefaultSeed)
	if err != nil {
		return nil, err
	}
	report := &LocalVerificationReport{
		Seed: DefaultSeed, SchemaVersions: []int{1, 2, 3},
		TraceCounts: map[int]int{}, Exceptions: map[string]*Error{},
		Incremental:  map[string]*IncrementalReport{},
		ActiveLayers: ActiveLayerIDs(bundle.Canonical),
	}
	for version := SchemaV1; version <= SchemaV3; version++ {
		loaded := bundle.Loaded[version]
		report.TraceCounts[version] = len(loaded.Traces.Fields)
		if _, err := CompareExpressions(bundle.Loaded[SchemaV3], loaded); err != nil {
			return report, err
		}
	}
	report.TreeEquivalent = true
	truthMismatches, err := VerifyTruth(bundle.Loaded[SchemaV3], bundle.Truth)
	if err != nil {
		return report, err
	}
	if len(truthMismatches) > 0 {
		return report, &Error{Code: CodeChunk, Message: "analytic truth verification failed", Details: truthMismatches}
	}
	report.TruthVerified = true
	for _, frame := range bundle.Truth.Frames {
		report.TruthSamples += len(frame.SampleWeights)
	}
	report.LossyV1 = bundle.ExportLosses[SchemaV1]

	report.Exceptions["E_SCHEMA_unknown_version"] = captureError(func() error {
		_, err := LoadProject([]byte(`{"version":9}`))
		return err
	})
	var v1 map[string]any
	if err := json.Unmarshal(bundle.Documents[SchemaV1], &v1); err == nil {
		delete(v1, "project_id")
		data, _ := json.Marshal(v1)
		report.Exceptions["E_SCHEMA_missing_required"] = captureError(func() error {
			_, err := LoadProject(data)
			return err
		})
	}
	report.Exceptions["E_OVF_tick_scale"] = captureError(func() error {
		_, err := TicksToNanoseconds("$.demo.overflow_ticks", math.MaxInt64, nil)
		return err
	})
	report.Exceptions["E_BUDGET_pixel_buffer"] = captureError(func() error {
		return NewPixelBuffer(1).use(1024)
	})
	report.Exceptions["E_TREE_first_difference"] = treeMismatchExample(bundle.Loaded[SchemaV3])

	threePaths, err := VerifyThreePaths(ctx, bundle.Loaded[SchemaV3], 0, func() CheckpointStore {
		return NewMemoryStore()
	})
	if err != nil {
		return report, err
	}
	report.ThreePaths = threePaths

	opacityProject := bundle.Loaded[SchemaV3]
	opacity, err := VerifyIncrementalEdit(ctx, opacityProject, 0, NewMemoryStore(), func(p *Project) (Change, error) {
		return SetLayerOpacity(p, "slate_badge", 4200)
	})
	if err != nil {
		return report, err
	}
	report.Incremental["opacity_badge"] = opacity

	groupProject := bundle.Loaded[SchemaV2]
	group, err := VerifyIncrementalEdit(ctx, groupProject, 0, NewMemoryStore(), func(p *Project) (Change, error) {
		return SetGroupVisibility(p, "overlay_group", false)
	})
	if err != nil {
		return report, err
	}
	report.Incremental["group_visibility"] = group

	rippleProject := bundle.Loaded[SchemaV1]
	ripple, err := VerifyIncrementalEdit(ctx, rippleProject, 0, NewMemoryStore(), func(p *Project) (Change, error) {
		return EditRippleSource(p, "r_left", nil, 0, 0, 4321)
	})
	if err != nil {
		return report, err
	}
	report.Incremental["ripple_source"] = ripple

	if sqliteConfig != nil {
		sqlReport := &SQLiteVerificationReport{Available: true, Driver: sqliteConfig.Driver, CheckpointDSN: sqliteConfig.CheckpointDSN}
		sqlPaths, err := VerifyThreePaths(ctx, bundle.Loaded[SchemaV3], 0, func() CheckpointStore {
			db, openErr := sqliteConfig.ThreePathOpener()
			if openErr != nil {
				panic(openErr)
			}
			return NewSQLStore(db)
		})
		if err != nil {
			return report, err
		}
		sqlReport.ThreePaths = sqlPaths
		db, err := sqliteConfig.IncrementalOpener()
		if err != nil {
			return report, err
		}
		fresh, genErr := GenerateSample(DefaultSeed)
		if genErr != nil {
			return report, genErr
		}
		sqlReport.Incremental, err = VerifyIncrementalEdit(ctx, fresh.Loaded[SchemaV3], 0, NewSQLStore(db), func(p *Project) (Change, error) {
			return SetLayerOpacity(p, "slate_badge", 3300)
		})
		if err != nil {
			return report, err
		}
		report.SQLite = sqlReport
	}
	return report, nil
}

func captureError(fn func() error) *Error {
	if err := fn(); err != nil {
		if structured, ok := AsError(err); ok {
			return structured
		}
		return &Error{Code: "UNEXPECTED", Message: err.Error()}
	}
	return nil
}

func treeMismatchExample(p *Project) *Error {
	other := *p
	other.Resolved = append([]ResolvedLayer(nil), p.Resolved...)
	if len(other.Resolved) >= 2 {
		other.Resolved[0], other.Resolved[1] = other.Resolved[1], other.Resolved[0]
	}
	_, err := CompareExpressions(p, &other)
	return captureError(func() error { return err })
}
