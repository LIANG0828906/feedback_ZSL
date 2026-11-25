package ocrflow

import (
	"fmt"
	"os"
	"path/filepath"
)

type VerificationCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Code   Code   `json:"code,omitempty"`
}

type VerificationReport struct {
	OK                bool                `json:"ok"`
	WorkDir           string              `json:"work_dir"`
	SampleDir         string              `json:"sample_dir"`
	Checks            []VerificationCheck `json:"checks"`
	TotalLines        int                 `json:"total_lines"`
	RegionCacheHits   int                 `json:"region_cache_hits"`
	RegionCacheMisses int                 `json:"region_cache_misses"`
	RecipeInvalidated int                 `json:"recipe_invalidated_lines"`
	RecoveryDiscarded []string            `json:"recovery_discarded"`
}

func RunVerification(workDir string) (*VerificationReport, error) {
	if workDir == "" {
		workDir = filepath.Join("artifacts", "demo")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}
	rep := &VerificationReport{OK: true, WorkDir: workDir, SampleDir: filepath.Join(workDir, "samples")}
	add := func(name string, ok bool, detail string, code Code) {
		rep.Checks = append(rep.Checks, VerificationCheck{Name: name, OK: ok, Detail: detail, Code: code})
		if !ok {
			rep.OK = false
		}
	}

	manifest, err := WriteSampleSet(rep.SampleDir, FixedSeed)
	if err != nil {
		return nil, err
	}
	verifiedManifest, err := VerifySampleSet(rep.SampleDir, FixedSeed)
	if err != nil {
		add("deterministic_samples", false, err.Error(), CodeSchema)
		return rep, nil
	}
	add("deterministic_samples", true, fmt.Sprintf("shots=%d files=%d recipes=%d", len(verifiedManifest.Shots), len(verifiedManifest.Files), len(verifiedManifest.Recipes)), "")

	loaded := map[string]*Recipe{}
	for _, rf := range manifest.Recipes {
		data, err := os.ReadFile(filepath.Join(rep.SampleDir, rf.Path))
		if err != nil {
			return nil, err
		}
		r, err := LoadRecipeDocument(data)
		if err != nil {
			add("recipe_load_"+rf.SourceVersion, false, err.Error(), CodeSchema)
			return rep, nil
		}
		loaded[rf.SourceVersion] = r
	}
	add("three_schema_recipes", len(loaded) == 3, "loaded v1/v2/v3 with field-path validation", "")

	conv, err := VerifyConversionRoundTrip(SampleRecipe())
	if err != nil {
		return nil, err
	}
	add("integer_roundtrip", conv.OK, fmt.Sprintf("items=%d threshold_tol=%dBPS coord_tol=%dpx", len(conv.Items), conv.ThresholdToleranceBPS, conv.CoordinateTolerancePx), CodeOverflow)

	shots, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		return nil, err
	}
	rep.TotalLines = totalLineCount(shots)

	recipe := loaded["v3"]
	fullDB := filepath.Join(workDir, "verify-full.db")
	removeDBFiles(fullDB)
	svc, err := NewService(DefaultConfig(fullDB), recipe, DefaultTemplateLibrary(), shots)
	if err != nil {
		oe, _ := asError(err)
		code := CodeParam
		if oe != nil {
			code = oe.Code
		}
		add("service_init", false, err.Error(), code)
		return rep, nil
	}
	fullRep, err := svc.Run("full-empty-cache")
	if err != nil {
		return nil, err
	}
	ocrOK := true
	cer := 0
	for _, sr := range fullRep.Shots {
		if sr.Status != "success" || len(sr.Lines) != len(sr.Accuracy.Lines) {
			ocrOK = false
		}
		cer += sr.Accuracy.TotalDistance
	}
	add("detect_and_recognize", ocrOK && cer == 0, fmt.Sprintf("lines=%d CER_distance=%d commits=%d", len(fullRep.LineSequence), cer, len(fullRep.CommitSequence)), "")

	_, toggleOK := ToggleBackgroundBit(shots[1], mustDetection(shots[1], recipe), 1)
	if !toggleOK {
		add("region_replacement", false, "no background mutation point found", CodeDiff)
	} else {
		_, err = svc.InvalidateRegion(shots[1].ShotID, 1, "region_replaced")
		if err != nil {
			return nil, err
		}
	}
	incRep, err := svc.Run("incremental-region")
	if err != nil {
		return nil, err
	}
	rep.RegionCacheHits = incRep.CacheHits
	rep.RegionCacheMisses = incRep.CacheMisses
	add("precise_region_invalidation", incRep.CacheHits == rep.TotalLines-1 && incRep.CacheMisses == 1,
		fmt.Sprintf("hits=%d misses=%d expected_hits=%d", incRep.CacheHits, incRep.CacheMisses, rep.TotalLines-1), "")

	compareDB := filepath.Join(workDir, "verify-full-after-region.db")
	removeDBFiles(compareDB)
	fullSvc, err := NewService(DefaultConfig(compareDB), recipe, DefaultTemplateLibrary(), shots)
	if err != nil {
		return nil, err
	}
	fullAfterRegion, err := fullSvc.Run("full-empty-cache-after-region")
	if err != nil {
		return nil, err
	}
	if err := CompareRunReports(fullAfterRegion, incRep); err != nil {
		oe, _ := asError(err)
		add("incremental_full_diff", false, err.Error(), oe.Code)
	} else {
		add("incremental_full_diff", true, "text, char confidence, boxes and line sequence equal", CodeDiff)
	}
	_ = svc.Close()
	_ = fullSvc.Close()

	recipeDB := filepath.Join(workDir, "verify-recipe.db")
	removeDBFiles(recipeDB)
	recipeSvc, err := NewService(DefaultConfig(recipeDB), DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		return nil, err
	}
	_, err = recipeSvc.Run("baseline")
	if err != nil {
		return nil, err
	}
	invalidated, err := recipeSvc.UpdateRecipe(func(r *Recipe) { r.Recognition.MinConfidenceBPS = 6100 }, "recognition.min_confidence_bps")
	if err != nil {
		return nil, err
	}
	recipeInc, err := recipeSvc.Run("recipe-incremental")
	if err != nil {
		return nil, err
	}
	rep.RecipeInvalidated = len(invalidated)
	recipeFullDB := filepath.Join(workDir, "verify-recipe-full.db")
	removeDBFiles(recipeFullDB)
	recipeFullSvc, err := NewService(DefaultConfig(recipeFullDB), recipeSvc.Recipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		return nil, err
	}
	recipeFullRep, err := recipeFullSvc.Run("full-with-edited-recipe")
	if err != nil {
		return nil, err
	}
	if err := CompareRunReports(recipeFullRep, recipeInc); err != nil {
		add("recipe_edit_incremental_diff", false, err.Error(), CodeDiff)
	} else {
		add("recipe_edit_incremental_diff", len(invalidated) == rep.TotalLines, fmt.Sprintf("invalidated=%d lines and full/incremental equal", len(invalidated)), CodeDiff)
	}
	_ = recipeSvc.Close()
	_ = recipeFullSvc.Close()

	recoveryDB := filepath.Join(workDir, "verify-recovery.db")
	removeDBFiles(recoveryDB)
	recSvc, err := NewService(DefaultConfig(recoveryDB), DefaultRecipe(), DefaultTemplateLibrary(), shots)
	if err != nil {
		return nil, err
	}
	healthyRep, err := recSvc.Run("before-kill")
	if err != nil {
		return nil, err
	}
	healthyRecovery, err := recSvc.RecoverCurrentCheckpoints()
	if err != nil {
		return nil, err
	}
	add("healthy_recovery_scan", len(healthyRecovery.DiscardedChunks) == 0, fmt.Sprintf("valid_chunks=%d", len(healthyRecovery.ValidChunks)), "")
	if err := recSvc.Store().debugInsertKilledTransaction(CheckpointChunk{ChunkID: "chunk:killed-demo:99", ShotID: "shot-0001", RecipeVersion: recipe.RecipeVersion, ParamVersion: recipe.ParamVersion, AlgorithmVersion: RecipeAlgorithmVersion, Lines: []CheckpointLine{{LineNo: 1, Ordinal: 1}}}); err != nil {
		return nil, err
	}
	_, _ = recSvc.store.db.Exec(`UPDATE checkpoint_lines SET param_version='tampered' WHERE rowid=(SELECT rowid FROM checkpoint_lines ORDER BY rowid LIMIT 1)`)
	recovery, err := recSvc.RecoverCurrentCheckpoints()
	if err != nil {
		return nil, err
	}
	rep.RecoveryDiscarded = recovery.DiscardedChunks
	recoveredRep, err := recSvc.Run("after-recovery")
	if err != nil {
		return nil, err
	}
	if err := CompareRunReports(healthyRep, recoveredRep); err != nil {
		add("forced_termination_recovery", false, err.Error(), CodeDiff)
	} else {
		add("forced_termination_recovery", len(recovery.DiscardedChunks) > 0, fmt.Sprintf("discarded=%v resume_after=%s killed_report=false", recovery.DiscardedChunks, recovery.ResumeAfterChunkID), "")
	}
	_ = recSvc.Close()

	for _, c := range rep.Checks {
		if !c.OK {
			rep.OK = false
			break
		}
	}
	return rep, nil
}

func totalLineCount(shots []*Screenshot) int {
	n := 0
	for _, s := range shots {
		n += len(s.Lines)
	}
	return n
}

func mustDetection(shot *Screenshot, recipe *Recipe) *DetectionResult {
	prep, err := PreprocessImage(shot.RGBA, recipe.Preprocess)
	if err != nil {
		panic(err)
	}
	det, err := DetectLines(shot, recipe, prep)
	if err != nil {
		panic(err)
	}
	return det
}
func removeDBFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
