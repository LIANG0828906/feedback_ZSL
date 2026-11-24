package pipeline

import (
	"context"
	"runtime"
	"sort"

	"cadpipe/internal/cad"
	"cadpipe/internal/render"
	"cadpipe/internal/store"
	"cadpipe/internal/tessellate"
)

type Options struct {
	Workers     int
	BufferCount int
	Store       *store.Store
}

type Pipeline struct {
	Scene    *cad.Scene
	Camera   *render.Camera
	Versions Versions
	Probes   []cad.ClipProbe

	Meshes      map[string]*tessellate.Mesh
	PatchTiles  map[string]map[int]bool
	TilePatches map[int]map[string]bool
	Cache       map[int]*render.TileResult

	Options Options
	Pool    *BufferPool
	Store   *store.Store

	CommitHook      func(tileID int, committed int)
	AfterRenderTile func(tileID int)
	lastCommitCount int
}

func New(scene *cad.Scene, cam *render.Camera, probes []cad.ClipProbe, opts Options) (*Pipeline, error) {
	return NewWithVersions(scene, cam, probes, opts, NewVersions(scene))
}

func NewWithVersions(scene *cad.Scene, cam *render.Camera, probes []cad.ClipProbe, opts Options, versions Versions) (*Pipeline, error) {
	if opts.Workers <= 0 {
		opts.Workers = minInt(4, runtime.GOMAXPROCS(0))
	}
	if opts.BufferCount <= 0 {
		opts.BufferCount = opts.Workers
	}
	pixelCap := cam.TileWidth * cam.TileHeight
	p := &Pipeline{
		Scene: scene, Camera: cam, Versions: versions, Probes: append([]cad.ClipProbe(nil), probes...),
		Meshes:      make(map[string]*tessellate.Mesh),
		PatchTiles:  make(map[string]map[int]bool),
		TilePatches: make(map[int]map[string]bool),
		Cache:       make(map[int]*render.TileResult),
		Options:     opts, Store: opts.Store,
		Pool: NewBufferPool(opts.BufferCount, pixelCap),
	}
	if err := p.RebuildCoverage(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Pipeline) RebuildCoverage() error {
	p.Meshes = make(map[string]*tessellate.Mesh)
	p.PatchTiles = make(map[string]map[int]bool)
	p.TilePatches = make(map[int]map[string]bool)
	for _, patch := range p.Scene.SortedPatches() {
		mesh, err := tessellate.Tessellate(patch, patch.Subdivision)
		if err != nil {
			return err
		}
		p.Meshes[patch.ID] = mesh
		tiles := render.ExactCoveredTiles(p.Camera, patch, mesh)
		p.PatchTiles[patch.ID] = tiles
		for tileID := range tiles {
			if p.TilePatches[tileID] == nil {
				p.TilePatches[tileID] = make(map[string]bool)
			}
			p.TilePatches[tileID][patch.ID] = true
		}
	}
	return nil
}

func (p *Pipeline) RefreshPatchCoverage(patchID string) (map[int]bool, error) {
	patch := p.Scene.Patch(patchID)
	mesh, err := tessellate.Tessellate(patch, patch.Subdivision)
	if err != nil {
		return nil, err
	}
	p.Meshes[patchID] = mesh
	newTiles := render.ExactCoveredTiles(p.Camera, patch, mesh)

	for tileID := range p.PatchTiles[patchID] {
		if ps := p.TilePatches[tileID]; ps != nil {
			delete(ps, patchID)
			if len(ps) == 0 {
				delete(p.TilePatches, tileID)
			}
		}
	}
	p.PatchTiles[patchID] = newTiles
	for tileID := range newTiles {
		if p.TilePatches[tileID] == nil {
			p.TilePatches[tileID] = make(map[string]bool)
		}
		p.TilePatches[tileID][patchID] = true
	}
	return newTiles, nil
}

func (p *Pipeline) Views() []render.PatchView {
	views := make([]render.PatchView, 0, len(p.Scene.Patches))
	for _, patch := range p.Scene.SortedPatches() {
		mesh := p.Meshes[patch.ID]
		if mesh == nil {
			var err error
			mesh, err = tessellate.Tessellate(patch, patch.Subdivision)
			if err != nil {
				continue
			}
			p.Meshes[patch.ID] = mesh
		}
		views = append(views, render.PatchView{
			Patch: patch, Mesh: mesh, MaterialVersion: p.Versions.Patches[patch.ID].Material,
		})
	}
	return views
}

func (p *Pipeline) ExpectedStoreTiles() map[int]store.ExpectedTile {
	out := make(map[int]store.ExpectedTile, p.Camera.TileCount())
	for id := 0; id < p.Camera.TileCount(); id++ {
		patchIDs := sortedKeys(p.TilePatches[id])
		out[id] = store.ExpectedTile{
			TileID: id, DependencyHash: p.DependencyHashForPatches(patchIDs),
			SceneVersion: p.Versions.SceneStructure, CameraVersion: p.Versions.Camera,
			AlgorithmVersion: tessellate.AlgorithmVersion,
		}
	}
	return out
}

func (p *Pipeline) Recover(ctx context.Context) (int, error) {
	if p.Store == nil {
		return 0, nil
	}
	rows, err := p.Store.LoadCompatible(ctx, p.ExpectedStoreTiles())
	if err != nil {
		return 0, err
	}
	for id, tile := range rows {
		p.Cache[id] = tile
	}
	return len(rows), nil
}

func (p *Pipeline) SortedTileIDs(ids []int) []int {
	if ids == nil {
		ids = make([]int, 0, p.Camera.TileCount())
		for id := 0; id < p.Camera.TileCount(); id++ {
			ids = append(ids, id)
		}
	}
	out := append([]int(nil), ids...)
	sort.Ints(out)
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = context.Background
