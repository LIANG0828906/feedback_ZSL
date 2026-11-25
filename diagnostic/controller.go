// Package diagnostic exposes the bake kernel through a local visual workbench
// shared by the pure-Go HTTP entry point and the optional Wails shell.
package diagnostic

import (
	"context"
	"fmt"
	"os"
	"sync"

	"terrainbake/bake"
	"terrainbake/checkpoint"
	"terrainbake/generator"
	"terrainbake/model"
	"terrainbake/project"
)

// Controller owns one deterministic diagnostic scene.
type Controller struct {
	mu            sync.Mutex
	seed          int64
	dbPath        string
	bundle        model.SampleBundle
	engine        *bake.Engine
	store         *checkpoint.Store
	baseline      *bake.BakeResult
	ordered       *bake.BakeResult
	incremental   *bake.BakeResult
	truthReport   *bake.TruthReport
	truthError    string
	comparison    ComparisonView
	invalidations []bake.Invalidation
	seam          *bake.SeamReport
	edited        bool
}

// New creates a scene, restores any compatible checkpoint, and performs the
// baseline, incremental, and any-order comparisons.
func New(seed int64, dbPath string) (*Controller, error) {
	c := &Controller{seed: seed, dbPath: dbPath}
	if err := c.initialize(); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *Controller) initialize() error {
	bundle, err := generator.Generate(c.seed)
	if err != nil {
		return err
	}
	if err := project.ValidateProject(&bundle.Project); err != nil {
		return err
	}
	c.bundle = bundle
	c.edited = false
	c.invalidations = nil

	if c.dbPath != "" {
		store, err := checkpoint.Open(context.Background(), c.dbPath, model.ProjectIdentity(bundle.Project), bundle.Project.Params)
		if err != nil {
			return err
		}
		c.store = store
	}
	engine, err := bake.NewEngine(&c.bundle.Project, c.store)
	if err != nil {
		return err
	}
	c.engine = engine
	if c.store != nil {
		if _, err := engine.LoadPersisted(context.Background()); err != nil {
			return err
		}
	}
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	baselineEngine, err := bake.NewEngine(&c.bundle.Project, nil)
	if err != nil {
		return err
	}
	c.baseline, err = baselineEngine.BakeFull(context.Background(), opts)
	if err != nil {
		return err
	}
	order := make([]string, 0, len(bundle.Project.Chunks))
	for i := len(bundle.Project.Chunks) - 1; i >= 0; i-- {
		order = append(order, bundle.Project.Chunks[i].ID)
	}
	orderedEngine, err := bake.NewEngine(&c.bundle.Project, nil)
	if err != nil {
		return err
	}
	c.ordered, err = orderedEngine.BakeFullOrder(context.Background(), order, opts)
	if err != nil {
		return err
	}
	c.incremental, err = engine.BakeIncremental(context.Background(), opts)
	if err != nil {
		return err
	}
	c.updateComparisons()
	return nil
}

func (c *Controller) updateComparisons() {
	c.comparison = ComparisonView{Equal: true}
	if _, err := bake.CompareResults(c.baseline, c.incremental); err != nil {
		c.comparison.Equal = false
		c.comparison.Error = "incremental: " + err.Error()
		c.comparison.Code = model.CodeOf(err)
		coded, _ := model.AsCoded(err)
		if coded != nil {
			c.comparison.ChunkID = coded.ChunkID
			c.comparison.Field = coded.Field
		}
	}
	if _, err := bake.CompareResults(c.baseline, c.ordered); err != nil {
		c.comparison.Equal = false
		c.comparison.Error = "any-order: " + err.Error()
		c.comparison.Code = model.CodeOf(err)
		coded, _ := model.AsCoded(err)
		if coded != nil {
			c.comparison.ChunkID = coded.ChunkID
			c.comparison.Field = coded.Field
		}
	}
	c.truthReport = nil
	c.truthError = ""
	if !c.edited {
		report, err := bake.CompareTruth(c.baseline, c.bundle.Truth, c.bundle.Project.Params.IrradianceTolerance)
		c.truthReport = report
		if err != nil {
			c.truthError = err.Error()
		}
	}
	if c.seam == nil {
		if report, err := bake.AnalyzeSeams(c.baseline, c.engine.Terrain()); err == nil {
			c.seam = report
		}
	}
}

// State returns the visual scene at one logical solar time.
func (c *Controller) State(timeValue int) State {
	c.mu.Lock()
	defer c.mu.Unlock()
	times := make([]int, 0, len(c.bundle.Project.Sun))
	found := false
	for _, s := range c.bundle.Project.Sun {
		times = append(times, s.Time)
		if s.Time == timeValue {
			found = true
		}
	}
	if !found && len(times) > 0 {
		timeValue = times[0]
	}
	state := State{
		Seed:          c.seed,
		Database:      c.dbPath,
		Time:          timeValue,
		Times:         times,
		Bounds:        c.bundle.Project.Bounds,
		Params:        c.bundle.Project.Params,
		Edges:         c.engine.Terrain().Edges(),
		Invalidations: append([]bake.Invalidation(nil), c.invalidations...),
		Comparison:    c.comparison,
		Truth:         c.truthReport,
		TruthError:    c.truthError,
		Edited:        c.edited,
		Stats:         c.incremental.Stats,
		Seam:          c.seam,
	}
	for _, chunk := range c.bundle.Project.Chunks {
		view := ChunkView{
			ID: chunk.ID, GridX: chunk.GridX, GridZ: chunk.GridZ, Bounds: chunk.Bounds,
			ResX: chunk.ResX, ResZ: chunk.ResZ, Heights: append([]float64(nil), chunk.Heights...),
			Albedo: chunk.Albedo,
		}
		if unit, ok := c.incremental.Unit(chunk.ID, timeValue); ok {
			view.Points = make([]PointView, 0, len(unit.Points))
			var sum [3]float64
			for _, p := range unit.Points {
				sum[0] += p.Irradiance[0]
				sum[1] += p.Irradiance[1]
				sum[2] += p.Irradiance[2]
				view.Points = append(view.Points, PointView{
					X: p.Vertex.X, Z: p.Vertex.Z,
					World: p.World, DirectShadow: p.DirectShadow,
					SkyVisibility: p.SkyVisibility, Irradiance: p.Irradiance,
					Steps: p.Shadow.Steps, Budget: p.Shadow.StepBudget,
					FirstHit: p.Shadow.FirstHit,
				})
			}
			if len(unit.Points) > 0 {
				for ch := 0; ch < 3; ch++ {
					view.AverageIrradiance[ch] = sum[ch] / float64(len(unit.Points))
				}
			}
		}
		state.Chunks = append(state.Chunks, view)
	}
	return state
}

// Raise edits one chunk vertex and re-runs incremental and fresh-full paths.
func (c *Controller) Raise(chunkID string, x, z int, delta float64) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	vertex := model.VertexPoint{X: x, Z: z}
	invalidations, err := c.engine.RaiseHeight(context.Background(), chunkID, vertex, delta)
	if err != nil {
		return State{}, err
	}
	c.invalidations = invalidations
	c.edited = true
	opts := bake.DefaultScheduleOptions(c.engine.Project().Params)
	c.incremental, err = c.engine.BakeIncremental(context.Background(), opts)
	if err != nil {
		return State{}, err
	}
	fullEngine, err := bake.NewEngine(c.engine.Project(), nil)
	if err != nil {
		return State{}, err
	}
	c.baseline, err = fullEngine.BakeFull(context.Background(), opts)
	if err != nil {
		return State{}, err
	}
	order := make([]string, 0, len(c.engine.Project().Chunks))
	for i := len(c.engine.Project().Chunks) - 1; i >= 0; i-- {
		order = append(order, c.engine.Project().Chunks[i].ID)
	}
	orderedEngine, err := bake.NewEngine(c.engine.Project(), nil)
	if err != nil {
		return State{}, err
	}
	c.ordered, err = orderedEngine.BakeFullOrder(context.Background(), order, opts)
	if err != nil {
		return State{}, err
	}
	c.seam = nil
	c.updateComparisons()
	return c.stateLocked(c.incremental.CommitSequence[0].Time), nil
}

// Reset discards the current checkpoint and recreates the deterministic scene.
func (c *Controller) Reset() (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close()
	if c.dbPath != "" {
		_ = os.Remove(c.dbPath)
		_ = os.Remove(c.dbPath + "-wal")
		_ = os.Remove(c.dbPath + "-shm")
	}
	c.store = nil
	c.engine = nil
	c.baseline = nil
	c.ordered = nil
	c.incremental = nil
	c.seam = nil
	if err := c.initialize(); err != nil {
		return State{}, err
	}
	return c.stateLocked(c.incremental.CommitSequence[0].Time), nil
}

func (c *Controller) stateLocked(timeValue int) State {
	c.mu.Unlock()
	state := c.State(timeValue)
	c.mu.Lock()
	return state
}

func (c *Controller) close() {
	if c.store != nil {
		_ = c.store.Close()
		c.store = nil
	}
}

// Close releases the checkpoint handle.
func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store != nil {
		err := c.store.Close()
		c.store = nil
		return err
	}
	return nil
}

// State is the diagnostic JSON contract.
type State struct {
	Seed          int64               `json:"seed"`
	Database      string              `json:"database,omitempty"`
	Time          int                 `json:"time"`
	Times         []int               `json:"times"`
	Bounds        model.Bounds        `json:"bounds"`
	Params        model.BakeParams    `json:"params"`
	Chunks        []ChunkView         `json:"chunks"`
	Edges         []model.SharedEdge  `json:"edges"`
	Invalidations []bake.Invalidation `json:"invalidations"`
	Comparison    ComparisonView      `json:"comparison"`
	Truth         *bake.TruthReport   `json:"truth,omitempty"`
	TruthError    string              `json:"truth_error,omitempty"`
	Edited        bool                `json:"edited"`
	Stats         bake.ScheduleStats  `json:"stats"`
	Seam          *bake.SeamReport    `json:"seam,omitempty"`
}

// ChunkView is one chunk's selected-time diagnostic data.
type ChunkView struct {
	ID                string       `json:"id"`
	GridX             int          `json:"grid_x"`
	GridZ             int          `json:"grid_z"`
	Bounds            model.Bounds `json:"bounds"`
	ResX              int          `json:"res_x"`
	ResZ              int          `json:"res_z"`
	Heights           []float64    `json:"heights"`
	Albedo            [3]float64   `json:"albedo"`
	AverageIrradiance [3]float64   `json:"average_irradiance"`
	Points            []PointView  `json:"points"`
}

// PointView is one sample's highlighted lighting state.
type PointView struct {
	X             int        `json:"x"`
	Z             int        `json:"z"`
	World         model.Vec3 `json:"world"`
	DirectShadow  bool       `json:"direct_shadow"`
	SkyVisibility float64    `json:"sky_visibility"`
	Irradiance    [3]float64 `json:"irradiance"`
	Steps         int        `json:"steps"`
	Budget        int        `json:"budget"`
	FirstHit      *model.Hit `json:"first_hit,omitempty"`
}

// ComparisonView summarizes the three-path equality checks.
type ComparisonView struct {
	Equal   bool   `json:"equal"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
	ChunkID string `json:"chunk_id,omitempty"`
	Field   string `json:"field,omitempty"`
}

// FormatStatus is a small CLI-friendly summary.
func (s State) FormatStatus() string {
	status := fmt.Sprintf("seed=%d time=%d chunks=%d edited=%t", s.Seed, s.Time, len(s.Chunks), s.Edited)
	if !s.Comparison.Equal {
		status += " comparison=" + s.Comparison.Code
	}
	if s.Truth != nil {
		status += fmt.Sprintf(" shadow_mismatch=%d max_sky=%.9g max_irr=%.9g",
			s.Truth.ShadowMismatches, s.Truth.MaxSkyError, s.Truth.MaxIrradianceError)
	}
	return status
}
