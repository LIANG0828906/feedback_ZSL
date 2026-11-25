package probe

import (
	"context"
	"sort"
	"sync"

	"lightprobe/internal/checkpoint"
	"lightprobe/internal/diag"
	"lightprobe/internal/scene"
)

type StateTransition struct {
	RoomID string    `json:"room_id"`
	From   RoomState `json:"from"`
	To     RoomState `json:"to"`
	Reason string    `json:"reason,omitempty"`
}

type Engine struct {
	mu sync.Mutex

	scene  *scene.Scene
	config Config

	metrics *Metrics
	faults  *FaultController
	graph   *DependencyGraph

	records map[string]*Record
	lattice map[string][][][]string
	order   []string

	pixelResults map[string]PixelResult
	roomStates   map[string]RoomState
	history      []StateTransition

	submissions        []Submission
	committedRoomOrder []string
	roomCommitSeq      map[string]int

	store      *checkpoint.Store
	sourceHash string

	lastPlan   *MutationPlan
	lastReport *RecomputeReport
}

func NewEngine(scn *scene.Scene, cfg Config) (*Engine, error) {
	if scn == nil {
		return nil, diag.New(diag.CodeSchema, "nil scene")
	}
	if err := scene.Validate(scn); err != nil {
		return nil, err
	}
	hash, err := checkpoint.HashJSON(scn)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		scene:         scn,
		config:        cfg.Normalize(),
		metrics:       &Metrics{},
		faults:        NewFaultController(),
		records:       make(map[string]*Record),
		lattice:       make(map[string][][][]string),
		pixelResults:  make(map[string]PixelResult),
		roomStates:    make(map[string]RoomState),
		roomCommitSeq: make(map[string]int),
		sourceHash:    hash,
	}
	for _, room := range scn.Rooms {
		e.roomStates[room.ID] = RoomPending
	}
	if err := e.rebuildTopologyLocked(nil, ""); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) SourceHash() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sourceHash
}

func (e *Engine) Config() Config { return e.config }

func (e *Engine) Scene() *scene.Scene { return e.scene }

func (e *Engine) Graph() *DependencyGraph {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.graph
}

func (e *Engine) MetricsEvents() []QueueEvent { return e.metrics.Events() }

func (e *Engine) LastPlan() *MutationPlan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastPlan
}

func (e *Engine) LastReport() *RecomputeReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastReport == nil {
		return nil
	}
	copyReport := *e.lastReport
	copyReport.QueueEvents = append([]QueueEvent{}, e.lastReport.QueueEvents...)
	return &copyReport
}

func (e *Engine) rebuildTopologyLocked(preserve map[string]*Record, resetRoomID string) error {
	orderedProbes := append([]scene.Probe{}, e.scene.Probes...)
	sort.SliceStable(orderedProbes, func(i, j int) bool {
		a, b := orderedProbes[i], orderedProbes[j]
		ra, ia, _ := e.scene.RoomByID(a.RoomID)
		rb, ib, _ := e.scene.RoomByID(b.RoomID)
		if ia != ib {
			return ia < ib
		}
		_ = ra
		_ = rb
		if a.I != b.I {
			return a.I < b.I
		}
		if a.J != b.J {
			return a.J < b.J
		}
		return a.K < b.K
	})

	e.records = make(map[string]*Record, len(orderedProbes))
	e.order = make([]string, 0, len(orderedProbes))
	e.lattice = make(map[string][][][]string)
	e.graph = NewDependencyGraph()

	for _, room := range e.scene.Rooms {
		grid := make([][][]string, room.Resolution.X)
		for i := range grid {
			grid[i] = make([][]string, room.Resolution.Y)
			for j := range grid[i] {
				grid[i][j] = make([]string, room.Resolution.Z)
			}
		}
		e.lattice[room.ID] = grid
		e.graph.AddNode(Node{ID: "room:" + room.ID, Kind: "room", RoomID: room.ID, Label: room.Name})
	}

	for _, probeInput := range orderedProbes {
		room, _, ok := e.scene.RoomByID(probeInput.RoomID)
		if !ok {
			return diag.Schema("room_id", "probe %s has unknown room", probeInput.ID)
		}
		if e.lattice[room.ID][probeInput.I][probeInput.J][probeInput.K] != "" {
			return diag.Geom(room.ID, room.Index, "ijk", "duplicate lattice index for %s", probeInput.ID)
		}
		e.lattice[room.ID][probeInput.I][probeInput.J][probeInput.K] = probeInput.ID
		e.order = append(e.order, probeInput.ID)

		var record *Record
		if preserve != nil && probeInput.RoomID != resetRoomID {
			if old, ok := preserve[probeInput.ID]; ok && old.Position == probeInput.Position {
				record = old
			}
		}
		if record == nil {
			record = &Record{
				ID: probeInput.ID, RoomID: probeInput.RoomID,
				I: probeInput.I, J: probeInput.J, K: probeInput.K,
				Position: probeInput.Position, InputRadiance: probeInput.Radiance,
				Radiance: probeInput.Radiance, VisibilitySamples: probeInput.VisibilitySamples,
				Status: ProbePending,
			}
		}
		e.records[probeInput.ID] = record

		cell, cellID := scene.PhotonCellIndex(e.scene.PhotonGrid, probeInput.Position)
		record.PhotonCell = cell
		e.graph.AddNode(Node{ID: RadianceSourceNode(probeInput.ID), Kind: "radiance", RoomID: room.ID, Label: probeInput.ID + ".radiance"})
		e.graph.AddNode(Node{ID: SHSourceNode(cellID), Kind: "sh", Label: cellID})
		e.graph.AddNode(Node{ID: ReconNode(probeInput.ID), Kind: "probe", RoomID: room.ID, Label: probeInput.ID})
		e.graph.AddDependency(ReconNode(probeInput.ID), RadianceSourceNode(probeInput.ID))
		e.graph.AddDependency(ReconNode(probeInput.ID), SHSourceNode(cellID))
	}

	for _, pixel := range e.scene.Pixels {
		if err := e.addPixelGraphLocked(pixel); err != nil {
			return err
		}
	}
	for _, boundary := range e.scene.Boundaries {
		if err := e.addBoundaryAnchorGraphLocked(boundary); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) addPixelGraphLocked(pixel scene.Pixel) error {
	queryID := "pixel-query:" + pixel.ID
	interpID := InterpNode(queryID)
	pixelID := PixelNode(pixel.ID)
	e.graph.AddNode(Node{ID: interpID, Kind: "interpolation", RoomID: pixel.RoomID, Label: queryID})
	e.graph.AddNode(Node{ID: pixelID, Kind: "pixel", RoomID: pixel.RoomID, Label: pixel.ID})
	e.graph.AddDependency(pixelID, interpID)

	corners, err := e.trilinearCornersByRoomIDLocked(pixel.RoomID, pixel.Position)
	if err != nil {
		return err
	}
	blend, hasBlend := scene.BoundarySampleAt(e.scene, pixel.RoomID, pixel.Position)
	if hasBlend {
		blendID := BlendNode(queryID)
		e.graph.AddNode(Node{ID: blendID, Kind: "cross_domain_blend", RoomID: pixel.RoomID, Label: pixel.BoundaryID})
		e.graph.AddDependency(interpID, blendID)
		for _, corner := range corners {
			e.graph.AddDependency(blendID, ReconNode(corner.ID))
		}
		otherCorners, err := e.trilinearCornersByRoomIDLocked(blend.Other.ID, blend.OtherAt)
		if err != nil {
			return err
		}
		for _, corner := range otherCorners {
			e.graph.AddDependency(blendID, ReconNode(corner.ID))
		}
	} else {
		for _, corner := range corners {
			e.graph.AddDependency(interpID, ReconNode(corner.ID))
		}
	}
	return nil
}

func (e *Engine) addBoundaryAnchorGraphLocked(boundary scene.BoundaryRule) error {
	var position scene.Vec3
	for _, truth := range e.scene.Truth.Jumps {
		if truth.BoundaryID == boundary.ID {
			position = truth.Position
			break
		}
	}
	if position == (scene.Vec3{}) {
		roomA, _, _ := e.scene.RoomByID(boundary.RoomA)
		position = roomA.Bounds.Min
		position[boundary.Axis] = boundary.Position
	}
	queryID := "boundary-anchor:" + boundary.ID
	blendID := BlendNode(queryID)
	e.graph.AddNode(Node{ID: blendID, Kind: "cross_domain_blend", RoomID: boundary.RoomA, Label: boundary.ID})

	cornersA, err := e.trilinearCornersByRoomIDLocked(boundary.RoomA, position)
	if err != nil {
		return err
	}
	for _, corner := range cornersA {
		e.graph.AddDependency(blendID, ReconNode(corner.ID))
	}
	cornersB, err := e.trilinearCornersByRoomIDLocked(boundary.RoomB, position)
	if err != nil {
		position[boundary.Axis] = boundary.Position
		cornersB, err = e.trilinearCornersByRoomIDLocked(boundary.RoomB, position)
		if err != nil {
			return err
		}
	}
	for _, corner := range cornersB {
		e.graph.AddDependency(blendID, ReconNode(corner.ID))
	}
	return nil
}

func (e *Engine) recordByIDLocked(id string) (*Record, error) {
	record, ok := e.records[id]
	if !ok {
		return nil, diag.New(diag.CodeSchema, "%s", "unknown probe "+id).WithID(id)
	}
	return record, nil
}

func (e *Engine) Record(id string) (*Record, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.recordByIDLocked(id)
	if err != nil {
		return nil, err
	}
	copyRecord := *r
	return &copyRecord, nil
}

func (e *Engine) Records() []Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Record, 0, len(e.order))
	for _, id := range e.order {
		out = append(out, *e.records[id])
	}
	return out
}

func (e *Engine) RoomStates() map[string]RoomState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]RoomState, len(e.roomStates))
	for k, v := range e.roomStates {
		out[k] = v
	}
	return out
}

func (e *Engine) StateHistory() []StateTransition {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]StateTransition{}, e.history...)
}

func (e *Engine) Store() *checkpoint.Store { return e.store }

func (e *Engine) SetStore(store *checkpoint.Store) { e.store = store }

func (e *Engine) Context() context.Context { return context.Background() }
