// Package demo runs the reproducible three-path acceptance scenario in one directory.
package demo

import (
	"context"
	"os"
	"path/filepath"

	"acousticfield/internal/acoustic"
	"acousticfield/internal/audit"
	"acousticfield/internal/checkpoint"
	"acousticfield/internal/deps"
	"acousticfield/internal/generator"
	"acousticfield/internal/rebuild"
)

const (
	AlphaChunk  = "C04"
	AlphaBand   = 1
	AlphaValue  = int64(650_000)
	EmissionSrc = "SRC-C01"
	EmissionAdd = int64(1_000_000)
)

type Result struct {
	Root       string
	FullDir    string
	RealDir    string
	RecoverDir string
	Comparison *audit.ReportComparison
	Audits     map[string]*audit.Result
}

func ApplyAlpha(level *acoustic.Level) deps.Edit {
	edit, err := rebuild.EditAbsorption(level, AlphaChunk, AlphaBand, AlphaValue)
	if err != nil {
		panic(err)
	}
	return edit
}

func ApplyEmission(level *acoustic.Level) deps.Edit {
	for i := range level.Sources {
		if level.Sources[i].ID != EmissionSrc {
			continue
		}
		emission := acoustic.CopyInt64s(level.Sources[i].EmissionQ)
		emission[0] += EmissionAdd
		edit, err := rebuild.EditSourceEmission(level, EmissionSrc, emission)
		if err != nil {
			panic(err)
		}
		return edit
	}
	panic("missing " + EmissionSrc)
}

func Run(root string) (*Result, error) {
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	originalLevel, originalTruth := generator.Generate(generator.DefaultSeed)
	if err := checkpoint.WriteJSONAtomic(filepath.Join(root, "sample.json"), struct {
		Level *acoustic.Level `json:"level"`
		Truth *acoustic.Truth `json:"truth"`
	}{originalLevel, originalTruth}); err != nil {
		return nil, err
	}

	fullDir := filepath.Join(root, "full")
	if err := runFull(fullDir); err != nil {
		return nil, err
	}
	realDir := filepath.Join(root, "realtime")
	realReport, err := runRealtime(realDir)
	if err != nil {
		return nil, err
	}
	recoverDir := filepath.Join(root, "recovery")
	if err := runRecovery(recoverDir); err != nil {
		return nil, err
	}

	comparison, err := audit.CompareReportDirs(fullDir, realDir, recoverDir)
	if err != nil {
		return nil, err
	}
	if err := checkpoint.WriteJSONAtomic(filepath.Join(root, "comparison.json"), comparison); err != nil {
		return nil, err
	}
	audits := map[string]*audit.Result{}
	for name, dir := range map[string]string{"full": fullDir, "realtime": realDir, "recovery": recoverDir} {
		result, err := audit.AuditDir(dir)
		if err != nil {
			return nil, err
		}
		audits[name] = result
		if err := checkpoint.WriteJSONAtomic(filepath.Join(root, "audit-"+name+".json"), result); err != nil {
			return nil, err
		}
	}
	if err := checkpoint.WriteJSONAtomic(filepath.Join(root, "report.json"), realReport); err != nil {
		return nil, err
	}
	return &Result{Root: root, FullDir: fullDir, RealDir: realDir, RecoverDir: recoverDir, Comparison: comparison, Audits: audits}, nil
}

func runFull(dir string) error {
	level, _ := generator.Generate(generator.DefaultSeed)
	ApplyAlpha(level)
	ApplyEmission(level)
	truth := generator.OracleForLevel(level)
	store, err := checkpoint.Open(dir)
	if err != nil {
		return err
	}
	svc := rebuild.NewService(level, truth, store)
	_, err = svc.Run(context.Background(), rebuild.RunOptions{RoundID: "full-final", Mode: "one-shot-full", Full: true})
	return err
}

func runRealtime(dir string) (*acoustic.Report, error) {
	level, truth := generator.Generate(generator.DefaultSeed)
	store, err := checkpoint.Open(dir)
	if err != nil {
		return nil, err
	}
	svc := rebuild.NewService(level, truth, store)
	base, err := svc.Run(context.Background(), rebuild.RunOptions{RoundID: "base", Mode: "realtime-baseline", Full: true})
	if err != nil {
		return nil, err
	}

	alphaEdit := ApplyAlpha(level)
	invalid1 := rebuild.DependencyGraph(base).Plan(alphaEdit)
	if err := checkpoint.WriteJSONAtomic(filepath.Join(filepath.Dir(dir), "invalidations-alpha.json"), invalid1); err != nil {
		return nil, err
	}
	svc.Truth = generator.OracleForLevel(level)
	alphaReport, err := svc.Run(context.Background(), rebuild.RunOptions{
		RoundID: "edit-alpha", Mode: "realtime-incremental", Invalidations: invalid1,
	})
	if err != nil {
		return nil, err
	}

	emissionEdit := ApplyEmission(level)
	invalid2 := rebuild.DependencyGraph(alphaReport).Plan(emissionEdit)
	if err := checkpoint.WriteJSONAtomic(filepath.Join(filepath.Dir(dir), "invalidations-emission.json"), invalid2); err != nil {
		return nil, err
	}
	svc.Truth = generator.OracleForLevel(level)
	final, err := svc.Run(context.Background(), rebuild.RunOptions{
		RoundID: "edit-emission", Mode: "realtime-incremental", Invalidations: invalid2,
	})
	if err != nil {
		return nil, err
	}
	return final, nil
}

func runRecovery(dir string) error {
	level, truth := generator.Generate(generator.DefaultSeed)
	store, err := checkpoint.Open(dir)
	if err != nil {
		return err
	}
	svc := rebuild.NewService(level, truth, store)
	base, err := svc.Run(context.Background(), rebuild.RunOptions{RoundID: "base", Mode: "recovery-baseline", Full: true})
	if err != nil {
		return err
	}

	alphaEdit := ApplyAlpha(level)
	emissionEdit := ApplyEmission(level)
	graph := rebuild.DependencyGraph(base)
	invalid := mergeInvalidations(graph.Plan(alphaEdit), graph.Plan(emissionEdit))
	svc.Truth = generator.OracleForLevel(level)

	ctx, cancel := context.WithCancel(context.Background())
	committed := 0
	_, _ = svc.Run(ctx, rebuild.RunOptions{
		RoundID: "crashed-edit", Mode: "recovery-before-kill", Invalidations: invalid,
		AfterCommit: func(chunkID string, txnSeq int64) error {
			committed++
			if committed == 2 {
				cancel()
			}
			return nil
		},
	})

	_, err = svc.Run(context.Background(), rebuild.RunOptions{
		RoundID: "crashed-edit", Mode: "recovery-recomputed", Recover: true, Invalidations: invalid,
	})
	return err
}

func mergeInvalidations(groups ...[]acoustic.Invalidation) []acoustic.Invalidation {
	byID := map[string]acoustic.Invalidation{}
	for _, group := range groups {
		for _, inv := range group {
			old, ok := byID[inv.ChunkID]
			if !ok {
				byID[inv.ChunkID] = inv
				continue
			}
			old.Reason += ";" + inv.Reason
			byID[inv.ChunkID] = old
		}
	}
	out := make([]acoustic.Invalidation, 0, len(byID))
	for _, inv := range byID {
		out = append(out, inv)
	}
	return out
}
