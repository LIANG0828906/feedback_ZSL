package quantforensics

import (
	"encoding/json"
	"path/filepath"
)

type InvalidationStep struct {
	Name           string             `json:"name"`
	EditPath       string             `json:"editPath"`
	ChangedNodes   []string           `json:"changedNodes"`
	Report         InvalidationReport `json:"report"`
	ComputedBlocks []string           `json:"computedBlocks"`
	ReusedBlocks   []string           `json:"reusedBlocks"`
	FailedBlocks   []string           `json:"failedBlocks"`
}

type InvalidationScenario struct {
	StorePath string             `json:"storePath"`
	Steps     []InvalidationStep `json:"steps"`
}

func cloneForEdit(p *Project) *Project { return cloneProject(p) }

// ApplyForensicEditSequence applies the same deterministic edit sequence used
// by diagnostics and three-path verification.
func ApplyForensicEditSequence(p *Project) *Project {
	edited := cloneForEdit(p)
	for i := range edited.Palette.Entries {
		if edited.Palette.Entries[i].ID == "purple" {
			edited.Palette.Entries[i].Color = RGB{R: 141, G: 32, B: 199}
		}
	}
	edited.Palette.Version = "palette-forensic-edit-sequence-v1"
	for i := range edited.Blocks {
		if edited.Blocks[i].ID == "shot-main-b10" {
			edited.Blocks[i].HasDitherOverride = true
			edited.Blocks[i].DitherPPM = 16_000 // 160 permyriad, integer amplitude 4/255.
		}
	}
	return edited
}

func RunInvalidationScenario(base *Project, checkpointDir string) (*InvalidationScenario, error) {
	path := filepath.Join(checkpointDir, "invalidation.sqlite")
	removeSQLiteFiles(path)
	store, err := OpenSQLiteStore(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	sm := NewStateMachine()
	p := cloneForEdit(base)
	initial := NewEngine(p, store, EngineOptions{Mode: "invalidation-initial", Workers: 4, MaxPendingBlocks: 2, StateMachine: sm, PanelID: "panel-dirty"})
	if _, err := initial.Run(); err != nil {
		return nil, err
	}
	scenario := &InvalidationScenario{StorePath: path}

	// Step 1: edit the orange palette entry referenced by generated dirty rows.
	quants := map[string]QuantOutput{}
	initialRecords, err := store.Records(p.ProjectID)
	if err != nil {
		return nil, err
	}
	for id, r := range LatestStageRecords(initialRecords) {
		_ = id
		if r.Stage != StageQuantize {
			continue
		}
		var q QuantOutput
		if err := json.Unmarshal(r.OutputJSON, &q); err != nil {
			return nil, err
		}
		quants[q.BlockID] = q
	}
	oldPalette := p.Palette
	newPalette := oldPalette
	for i := range newPalette.Entries {
		if newPalette.Entries[i].ID == "orange" {
			newPalette.Entries[i].Color = RGB{R: 255, G: 150, B: 105}
		}
	}
	affectedPalette := AnalyzePaletteEdit(p, quants, 4, newPalette)
	graph := BuildDependencyGraph(p)
	invPalette := graph.InvalidateBlocks(p, affectedPalette, false)
	invPalette.ChangedNodes = []string{PaletteNodeID(4)}
	if err := invPalette.ApplyToStateMachine(sm); err != nil {
		return nil, err
	}
	if err := sm.TransitionPanel("panel-dirty", StatePending); err != nil {
		return nil, err
	}
	p.Palette = newPalette
	p.Palette.Version = "palette-orange-edit-v2"
	runPalette := NewEngine(p, store, EngineOptions{Mode: "palette-invalidation", Workers: 4, ReuseCheckpoints: true, Invalidation: &invPalette, StateMachine: sm, PanelID: "panel-dirty"})
	repPalette, err := runPalette.Run()
	if err != nil {
		return nil, err
	}
	scenario.Steps = append(scenario.Steps, InvalidationStep{
		Name: "palette_entry_orange", EditPath: "$.palette.entries[4].color", ChangedNodes: invPalette.ChangedNodes,
		Report: invPalette, ComputedBlocks: repPalette.ComputedBlocks, ReusedBlocks: repPalette.ReusedBlocks, FailedBlocks: repPalette.FailedBlocks,
	})

	// Step 2: edit one per-block dither field referenced by dirty records.
	ditherBlockID := "shot-panel-b10"
	affectedDither := map[string]string{ditherBlockID: "effective per-block dither parameter changed"}
	for i := range p.Blocks {
		if p.Blocks[i].ID == ditherBlockID {
			p.Blocks[i].HasDitherOverride = true
			p.Blocks[i].DitherPPM = 24_000
		}
	}
	graph = BuildDependencyGraph(p)
	invDither := graph.InvalidateBlocks(p, affectedDither, false)
	invDither.ChangedNodes = []string{DitherBlockNodeID(ditherBlockID)}
	if err := invDither.ApplyToStateMachine(sm); err != nil {
		return nil, err
	}
	if err := sm.TransitionPanel("panel-dirty", StatePending); err != nil {
		return nil, err
	}
	runDither := NewEngine(p, store, EngineOptions{Mode: "dither-invalidation", Workers: 2, ReuseCheckpoints: true, Invalidation: &invDither, StateMachine: sm, PanelID: "panel-dirty"})
	repDither, err := runDither.Run()
	if err != nil {
		return nil, err
	}
	scenario.Steps = append(scenario.Steps, InvalidationStep{
		Name: "per_block_dither", EditPath: "$.blocks[shot-panel-b10].ditherPermyriad", ChangedNodes: invDither.ChangedNodes,
		Report: invDither, ComputedBlocks: repDither.ComputedBlocks, ReusedBlocks: repDither.ReusedBlocks, FailedBlocks: repDither.FailedBlocks,
	})

	// Step 3: actual coordinate edit for the generated geometry dirty block.
	geoBlockID := "shot-atlas-b09"
	affectedGeo := map[string]string{geoBlockID: "absolute block rectangle and derived block coordinate changed"}
	for i := range p.Blocks {
		if p.Blocks[i].ID == geoBlockID {
			p.Blocks[i].Col = 2
			p.Blocks[i].X = 16
			p.Blocks[i].ResidualX = 0
		}
	}
	graph = BuildDependencyGraph(p)
	invGeo := graph.InvalidateBlocks(p, affectedGeo, true)
	invGeo.ChangedNodes = []string{GeometryBlockNodeID(geoBlockID)}
	if err := invGeo.ApplyToStateMachine(sm); err != nil {
		return nil, err
	}
	if err := sm.TransitionPanel("panel-dirty", StatePending); err != nil {
		return nil, err
	}
	runGeo := NewEngine(p, store, EngineOptions{Mode: "geometry-invalidation", Workers: 2, ReuseCheckpoints: true, Invalidation: &invGeo, StateMachine: sm, PanelID: "panel-dirty"})
	repGeo, err := runGeo.Run()
	if err != nil {
		return nil, err
	}
	scenario.Steps = append(scenario.Steps, InvalidationStep{
		Name: "block_coordinate", EditPath: "$.blocks[shot-atlas-b09].col", ChangedNodes: invGeo.ChangedNodes,
		Report: invGeo, ComputedBlocks: repGeo.ComputedBlocks, ReusedBlocks: repGeo.ReusedBlocks, FailedBlocks: repGeo.FailedBlocks,
	})
	return scenario, nil
}
