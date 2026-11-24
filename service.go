package terrain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

type ServiceOptions struct {
	DBPath     string
	Store      *Store
	SnapshotID string
	Budget     BudgetConfig
	Now        func() time.Time
}

type CacheEntry struct {
	Mesh           *Mesh        `json:"mesh,omitempty"`
	ContentHash    string       `json:"content_hash"`
	VertexDigest   string       `json:"vertex_digest"`
	TriangleDigest string       `json:"triangle_digest"`
	Dependencies   []Dependency `json:"dependencies"`
	Bytes          int64        `json:"bytes"`
	Status         string       `json:"status"`
	Failure        string       `json:"failure,omitempty"`
	BuildMS        int64        `json:"build_ms,omitempty"`
	UpdatedAt      string       `json:"updated_at,omitempty"`
}

type RunManifest struct {
	SnapshotID       string             `json:"snapshot_id"`
	StartedAt        string             `json:"started_at"`
	FinishedAt       string             `json:"finished_at,omitempty"`
	TopologicalOrder []int              `json:"topological_order"`
	Invalidation     []InvalidationItem `json:"invalidation"`
	Blocks           []BlockRunStat     `json:"blocks"`
	CommitOrder      []int              `json:"commit_order_this_run"`
	SnapshotSequence []CommitRecord     `json:"snapshot_sequence"`
	RecoveryBlocks   []int              `json:"recovery_blocks"`
	Budget           BudgetOccupancy    `json:"budget"`
	Queue            []QueueEvent       `json:"queue"`
	Counts           map[string]int     `json:"counts"`
}

type Service struct {
	proj           *Project
	opts           ServiceOptions
	store          *Store
	ownsStore      bool
	snapshotID     string
	meter          *BudgetMeter
	graph          *DependencyGraph
	mu             sync.Mutex
	runMu          sync.Mutex
	commitMu       sync.Mutex
	cache          map[int]*CacheEntry
	reports        map[string]*BoundaryReport
	manifest       *RunManifest
	faultOnce      map[int]error
	panicOnce      map[int]bool
	versionReset   bool
	recoveryBlocks []int
}

func NewService(proj *Project, opts ServiceOptions) (*Service, error) {
	if err := ValidateProject(proj); err != nil {
		return nil, err
	}
	cfg := opts.Budget
	if cfg.MaxParallel == 0 && cfg.PerWorkerVertexBuffer == 0 && cfg.MeshMemoryBudgetBytes == 0 {
		cfg = DefaultBudgetConfig()
	}
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = DefaultBudgetConfig().MaxParallel
	}
	if cfg.PerWorkerVertexBuffer <= 0 {
		cfg.PerWorkerVertexBuffer = DefaultBudgetConfig().PerWorkerVertexBuffer
	}
	if cfg.MeshMemoryBudgetBytes <= 0 {
		cfg.MeshMemoryBudgetBytes = DefaultBudgetConfig().MeshMemoryBudgetBytes
	}
	store := opts.Store
	owns := false
	var err error
	if store == nil {
		dbPath := opts.DBPath
		if dbPath == "" {
			dbPath = ":memory:"
		}
		store, err = OpenStore(dbPath)
		if err != nil {
			return nil, err
		}
		owns = true
	}
	snapshotID := opts.SnapshotID
	if snapshotID == "" {
		snapshotID = "snapshot-" + proj.ProjectID
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	s := &Service{
		proj: proj, opts: opts, store: store, ownsStore: owns, snapshotID: snapshotID,
		meter: NewBudgetMeter(cfg), graph: BuildDependencyGraph(proj),
		cache: map[int]*CacheEntry{}, reports: map[string]*BoundaryReport{},
		faultOnce: map[int]error{}, panicOnce: map[int]bool{},
	}

	algo, param, exists, err := store.SnapshotVersions(snapshotID)
	if err != nil {
		if owns {
			_ = store.Close()
		}
		return nil, err
	}
	if exists && (algo != AlgorithmVersion || param != proj.ParameterVersion) {
		if err := store.ResetSnapshot(snapshotID); err != nil {
			return nil, err
		}
		s.versionReset = true
	}
	if err := store.EnsureSnapshot(snapshotID, proj.ProjectID, AlgorithmVersion, proj.ParameterVersion, now()); err != nil {
		return nil, err
	}
	if !s.versionReset {
		if err := s.recover(now); err != nil {
			var coded *CodedError
			if errors.As(err, &coded) {
				if resetErr := store.ResetSnapshot(snapshotID); resetErr != nil {
					return nil, resetErr
				}
				s.versionReset = true
				s.cache = map[int]*CacheEntry{}
				if ensureErr := store.EnsureSnapshot(snapshotID, proj.ProjectID, AlgorithmVersion, proj.ParameterVersion, now()); ensureErr != nil {
					return nil, ensureErr
				}
			} else {
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Service) recover(now func() time.Time) error {
	loaded, _, err := s.store.LoadBlocks(s.snapshotID, AlgorithmVersion, s.proj.ParameterVersion)
	if err != nil {
		return err
	}
	for no, lb := range loaded {
		if no < 0 || no >= len(s.proj.Blocks) {
			return NewSchemaError("checkpoint.blocks", &no, "persisted block outside project")
		}
		deps := s.DependenciesFor(no)
		wantHash := MeshContentHash(s.proj.ProjectID, s.proj.Blocks[no], lb.Mesh, deps)
		if lb.ContentHash != wantHash || lb.VertexDigest != VertexDigest(lb.Mesh.Vertices) || lb.TriangleDigest != TriangleDigest(lb.Mesh.Triangles) {
			return &CodedError{Code: CodeSchema, Path: "checkpoint.blocks", BlockNo: &no, Message: "checkpoint hash or digest mismatch"}
		}
		bytes := ActualMeshBytes(lb.Mesh)
		s.cache[no] = &CacheEntry{
			Mesh: lb.Mesh, ContentHash: lb.ContentHash, VertexDigest: lb.VertexDigest, TriangleDigest: lb.TriangleDigest,
			Dependencies: deps, Bytes: bytes, Status: StatusRecovered, UpdatedAt: lb.CommittedAt,
		}
		s.recoveryBlocks = append(s.recoveryBlocks, no)
		s.meter.AddCached(bytes)
	}
	sort.Ints(s.recoveryBlocks)
	return nil
}

func (s *Service) Close() error {
	if s.ownsStore {
		return s.store.Close()
	}
	return nil
}

func (s *Service) SnapshotID() string { return s.snapshotID }

func (s *Service) VersionReset() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.versionReset
}

func (s *Service) RecoveryBlocks() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.recoveryBlocks...)
}

func (s *Service) DependenciesFor(blockNo int) []Dependency {
	var deps []Dependency
	for _, d := range s.graph.Dependencies {
		if d.BlockNo == blockNo {
			deps = append(deps, d)
		}
	}
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].BoundaryID != deps[j].BoundaryID {
			return deps[i].BoundaryID < deps[j].BoundaryID
		}
		return deps[i].DependsOn < deps[j].DependsOn
	})
	return deps
}

func (s *Service) InjectFailureOnce(blockNo int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		err = fmt.Errorf("injected transient block %d failure", blockNo)
	}
	s.faultOnce[blockNo] = err
}

func (s *Service) InjectPanicOnce(blockNo int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panicOnce[blockNo] = true
}

func (s *Service) RunIncremental(ctx context.Context, changes []BlockChange) (*RunManifest, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.runLocked(ctx, changes, false, nil)
}

func (s *Service) RebuildAll(ctx context.Context) (*RunManifest, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.runLocked(ctx, nil, true, nil)
}

func (s *Service) RetryBlock(ctx context.Context, blockNo int) (*RunManifest, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if blockNo < 0 || blockNo >= len(s.proj.Blocks) {
		return nil, NewSchemaError("retry.block_no", &blockNo, "block outside project")
	}
	return s.runLocked(ctx, nil, false, &blockNo)
}

type blockOutcome struct {
	mesh    *Mesh
	buildMS int64
	bytes   int64
	err     error
}

func (s *Service) runLocked(ctx context.Context, changes []BlockChange, rebuildAll bool, retry *int) (*RunManifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	proj := s.proj
	manifest := &RunManifest{SnapshotID: s.snapshotID, StartedAt: started.UTC().Format(time.RFC3339Nano), RecoveryBlocks: append([]int{}, s.recoveryBlocks...)}

	invalid := map[int]bool{}
	var plan *InvalidationPlan
	if retry != nil {
		invalid[*retry] = true
		plan = &InvalidationPlan{}
		for i := range proj.Blocks {
			item := InvalidationItem{BlockNo: i, Reason: "unchanged", Reuse: ReuseCache}
			if i == *retry {
				item = InvalidationItem{BlockNo: i, Invalid: true, Reason: "manual_retry", Reuse: ReuseRebuild}
			}
			plan.Items = append(plan.Items, item)
		}
	} else {
		var err error
		plan, err = PlanInvalidation(proj, changes)
		if err != nil {
			return nil, err
		}
		for _, item := range plan.Items {
			if item.Invalid {
				invalid[item.BlockNo] = true
			}
		}
	}
	if rebuildAll {
		invalid = map[int]bool{}
		for i := range proj.Blocks {
			invalid[i] = true
		}
	}

	s.mu.Lock()
	for i := range proj.Blocks {
		if !invalid[i] {
			entry := s.cache[i]
			if rebuildAll || entry == nil || entry.Mesh == nil {
				invalid[i] = true
				for k := range plan.Items {
					if plan.Items[k].BlockNo == i {
						plan.Items[k].Invalid = true
						plan.Items[k].Reason = "cache_missing"
						plan.Items[k].Reuse = ReuseRebuild
					}
				}
			}
		}
	}
	s.mu.Unlock()

	invalidList := make([]int, 0, len(invalid))
	for no := range invalid {
		invalidList = append(invalidList, no)
	}
	sort.Ints(invalidList)
	order, err := TopologicalOrder(proj, s.graph, invalid)
	if err != nil {
		return nil, err
	}
	if len(order) == 0 && len(invalidList) > 0 {
		order = invalidList
	}
	manifest.Invalidation = plan.Items
	manifest.TopologicalOrder = order

	alignPlan, err := PlanAlignment(proj)
	if err != nil {
		return nil, err
	}

	outcomes := s.buildParallel(ctx, proj, order, alignPlan)
	merged := map[int]*Mesh{}
	s.mu.Lock()
	for no, entry := range s.cache {
		if entry != nil && entry.Mesh != nil {
			merged[no] = entry.Mesh
		}
	}
	s.mu.Unlock()
	for _, no := range order {
		out := outcomes[no]
		if out.err == nil {
			merged[no] = out.mesh
		}
	}

	reports := map[string]*BoundaryReport{}
	for _, bnd := range proj.SharedBoundaries {
		ma, mb := merged[bnd.BlockA], merged[bnd.BlockB]
		if ma == nil || mb == nil {
			continue
		}
		c, err := findConstraint(proj, bnd.ConstraintID)
		if err != nil {
			return nil, err
		}
		sa := ExtractBoundarySide(ma, proj.Blocks[bnd.BlockA], bnd.SideA)
		sb := ExtractBoundarySide(mb, proj.Blocks[bnd.BlockB], bnd.SideB)
		buffer := c.AlignmentBufferCells * maxFloat(proj.Blocks[bnd.BlockA].Grid.CellSize, proj.Blocks[bnd.BlockB].Grid.CellSize)
		if buffer <= 1e-9 {
			buffer = 1e-6
		}
		reports[bnd.ID] = BuildBoundaryReport(BoundarySharedView{ID: bnd.ID, BlockA: bnd.BlockA, BlockB: bnd.BlockB}, sa, sb,
			c.MaxFoldAngleDeg, proj.Blocks[bnd.BlockA].ErrorTolerance, proj.Blocks[bnd.BlockB].ErrorTolerance, buffer)
	}

	statsByBlock := map[int]*BlockRunStat{}
	for i := range proj.Blocks {
		entry := s.cache[i]
		stat := BlockRunStat{BlockNo: i, Status: StatusClean, CacheHit: true}
		if entry != nil && entry.Mesh != nil {
			stat.Vertices = len(entry.Mesh.Vertices)
			stat.Triangles = len(entry.Mesh.Triangles)
			stat.MaxError = entry.Mesh.MaxHeightError
			stat.ContentHash = entry.ContentHash
			stat.BuildMS = entry.BuildMS
		}
		if entry != nil && entry.Status == StatusFailed {
			stat.Status = StatusFailed
			stat.Failure = entry.Failure
		}
		statsByBlock[i] = &stat
	}

	commitOrder := []int{}
	for _, no := range order {
		out := outcomes[no]
		stat := statsByBlock[no]
		stat.CacheHit = false
		if out.err != nil {
			stat.Status = StatusFailed
			stat.Failure = out.err.Error()
			s.mu.Lock()
			if old := s.cache[no]; old != nil {
				old.Status = StatusFailed
				old.Failure = out.err.Error()
			} else {
				s.cache[no] = &CacheEntry{Status: StatusFailed, Failure: out.err.Error()}
			}
			s.mu.Unlock()
			continue
		}

		deps := s.DependenciesFor(no)
		vd := VertexDigest(out.mesh.Vertices)
		td := TriangleDigest(out.mesh.Triangles)
		hash := MeshContentHash(proj.ProjectID, proj.Blocks[no], out.mesh, deps)
		var boundaryStats []*BoundaryReport
		for _, bnd := range proj.SharedBoundaries {
			if bnd.BlockA == no || bnd.BlockB == no {
				if rep := reports[bnd.ID]; rep != nil {
					boundaryStats = append(boundaryStats, rep)
				}
			}
		}
		s.commitMu.Lock()
		commitErr := s.store.CommitBlock(CommitBlockRequest{
			SnapshotID: s.snapshotID, ProjectID: proj.ProjectID, BlockNo: no, Mesh: out.mesh,
			Dependencies: deps, BoundaryStats: boundaryStats, VertexDigest: vd, TriangleDigest: td, ContentHash: hash,
			ParameterVersion: proj.ParameterVersion, AlgorithmVersion: AlgorithmVersion, CommittedAt: time.Now(),
		})
		s.commitMu.Unlock()
		if commitErr != nil {
			s.meter.ReleaseCached(out.bytes)
			stat.Status = StatusFailed
			stat.Failure = commitErr.Error()
			continue
		}

		var oldBytes int64
		s.mu.Lock()
		if old := s.cache[no]; old != nil {
			oldBytes = old.Bytes
		}
		nowText := time.Now().UTC().Format(time.RFC3339Nano)
		s.cache[no] = &CacheEntry{Mesh: out.mesh, ContentHash: hash, VertexDigest: vd, TriangleDigest: td, Dependencies: deps,
			Bytes: out.bytes, Status: StatusClean, BuildMS: out.buildMS, UpdatedAt: nowText}
		s.reports = reports
		s.mu.Unlock()
		if oldBytes > 0 {
			s.meter.ReleaseCached(oldBytes)
		}
		commitOrder = append(commitOrder, no)
		stat.Status = StatusClean
		stat.Failure = ""
		stat.Vertices = len(out.mesh.Vertices)
		stat.Triangles = len(out.mesh.Triangles)
		stat.MaxError = out.mesh.MaxHeightError
		stat.ContentHash = hash
		stat.BuildMS = out.buildMS
	}

	if retry != nil {
		for _, dep := range s.DependenciesFor(*retry) {
			neighbor := dep.DependsOn
			entry := s.cache[neighbor]
			if entry == nil || entry.Mesh == nil {
				continue
			}
			var boundaryStats []*BoundaryReport
			for _, bnd := range proj.SharedBoundaries {
				if bnd.BlockA == neighbor || bnd.BlockB == neighbor {
					if rep := reports[bnd.ID]; rep != nil {
						boundaryStats = append(boundaryStats, rep)
					}
				}
			}
			s.commitMu.Lock()
			err := s.store.CommitBlock(CommitBlockRequest{
				SnapshotID: s.snapshotID, ProjectID: proj.ProjectID, BlockNo: neighbor, Mesh: entry.Mesh,
				Dependencies: entry.Dependencies, BoundaryStats: boundaryStats, VertexDigest: entry.VertexDigest,
				TriangleDigest: entry.TriangleDigest, ContentHash: entry.ContentHash,
				ParameterVersion: proj.ParameterVersion, AlgorithmVersion: AlgorithmVersion,
			})
			s.commitMu.Unlock()
			if err == nil {
				st := statsByBlock[neighbor]
				st.CacheHit = true
				st.Status = StatusClean
			}
		}
	}

	sequence, seqErr := s.store.LoadSequence(s.snapshotID)
	if seqErr != nil {
		return nil, seqErr
	}
	manifest.CommitOrder = commitOrder
	manifest.SnapshotSequence = sequence
	manifest.Budget = s.meter.Occupancy()
	manifest.Queue = s.meter.Events()
	manifest.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	for i := range proj.Blocks {
		manifest.Blocks = append(manifest.Blocks, *statsByBlock[i])
	}
	manifest.Counts = map[string]int{}
	for _, st := range manifest.Blocks {
		if st.CacheHit {
			manifest.Counts["cache_hits"]++
		} else if st.Status == StatusClean {
			manifest.Counts["rebuilt"]++
		}
		if st.Status == StatusFailed {
			manifest.Counts["failed"]++
		}
	}
	s.mu.Lock()
	s.manifest = manifest
	s.reports = reports
	s.mu.Unlock()
	if manifest.Counts["failed"] > 0 {
		return manifest, fmt.Errorf("%d block(s) failed; see manifest", manifest.Counts["failed"])
	}
	_ = seqErr
	return manifest, nil
}

type scheduledOutcome struct {
	mesh         *Mesh
	buildMS      int64
	bytes        int64
	err          error
	leaseRelease func(actualBytes int64)
}

func (s *Service) buildParallel(ctx context.Context, proj *Project, order []int, plan *AlignmentPlan) map[int]scheduledOutcome {
	chans := map[int]chan scheduledOutcome{}
	for _, no := range order {
		ch := make(chan scheduledOutcome, 1)
		chans[no] = ch
		go func(blockNo int) {
			block := proj.Blocks[blockNo]
			worstVertices := block.Grid.Width * block.Grid.Height
			lease, acqErr := s.meter.Acquire(ctx, blockNo, worstVertices, EstimateBlockBytes(block))
			if acqErr != nil {
				ch <- scheduledOutcome{err: acqErr}
				return
			}
			out := scheduledOutcome{leaseRelease: func(actualBytes int64) {
				if actualBytes == 0 {
					lease.Release()
				} else {
					lease.Complete(actualBytes)
				}
			}}
			defer func() {
				if r := recover(); r != nil {
					lease.Release()
					out.err = fmt.Errorf("block %d internal exception: %v", blockNo, r)
					select {
					case ch <- out:
					default:
					}
				}
			}()
			s.mu.Lock()
			if s.panicOnce[blockNo] {
				delete(s.panicOnce, blockNo)
				s.mu.Unlock()
				panic(fmt.Sprintf("injected panic in block %d", blockNo))
			}
			if ferr := s.faultOnce[blockNo]; ferr != nil {
				delete(s.faultOnce, blockNo)
				s.mu.Unlock()
				lease.Release()
				out.err = ferr
				ch <- out
				return
			}
			s.mu.Unlock()

			start := time.Now()
			mesh, err := BuildBlockMesh(block, BuildOptions{ParameterVersion: proj.ParameterVersion, ForcedPoints: plan.Forced[blockNo]})
			out.buildMS = time.Since(start).Milliseconds()
			if err != nil {
				lease.Release()
				out.err = err
				ch <- out
				return
			}
			out.mesh = mesh
			out.bytes = ActualMeshBytes(mesh)
			lease.Complete(out.bytes)
			out.leaseRelease = func(int64) {}
			ch <- out
		}(no)
	}

	out := map[int]scheduledOutcome{}
	for no, ch := range chans {
		out[no] = <-ch
	}
	return out
}

func (s *Store) LoadSequence(snapshotID string) ([]CommitRecord, error) {
	rows, err := s.db.Query(`SELECT seq, block_no, content_hash, committed_at FROM commit_sequence WHERE snapshot_id=? ORDER BY seq`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitRecord
	for rows.Next() {
		var rec CommitRecord
		if err := rows.Scan(&rec.Seq, &rec.BlockNo, &rec.Hash, &rec.CommittedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

type EditRequest struct {
	GridX          *int     `json:"grid_x,omitempty"`
	GridY          *int     `json:"grid_y,omitempty"`
	HeightDelta    *float64 `json:"height_delta,omitempty"`
	Height         *float64 `json:"height,omitempty"`
	SampleBudget   *int     `json:"sample_budget,omitempty"`
	ErrorTolerance *float64 `json:"error_tolerance,omitempty"`
}

func (s *Service) ApplyEdit(ctx context.Context, blockNo int, req EditRequest) (*RunManifest, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if blockNo < 0 || blockNo >= len(s.proj.Blocks) {
		return nil, NewSchemaError("edit.block_no", &blockNo, "block outside project")
	}
	var changes []BlockChange
	s.mu.Lock()
	b := &s.proj.Blocks[blockNo]
	if req.SampleBudget != nil {
		if *req.SampleBudget <= 0 {
			s.mu.Unlock()
			return nil, NewSchemaError(fmt.Sprintf("blocks[%d].sample_budget", blockNo), &blockNo, "must be positive")
		}
		b.SampleBudget = *req.SampleBudget
		changes = append(changes, BlockChange{BlockNo: blockNo, Kind: ReasonBudget})
	}
	if req.ErrorTolerance != nil {
		if *req.ErrorTolerance < 0 {
			s.mu.Unlock()
			return nil, NewSchemaError(fmt.Sprintf("blocks[%d].error_tolerance", blockNo), &blockNo, "must be non-negative")
		}
		b.ErrorTolerance = *req.ErrorTolerance
		changes = append(changes, BlockChange{BlockNo: blockNo, Kind: ReasonTolerance})
	}
	if req.HeightDelta != nil || req.Height != nil {
		x, y := 0, 0
		if req.GridX != nil && req.GridY != nil {
			x, y = *req.GridX, *req.GridY
		}
		if x < 0 || y < 0 || x >= b.Grid.Width || y >= b.Grid.Height {
			s.mu.Unlock()
			return nil, NewSchemaError(fmt.Sprintf("blocks[%d].edit.grid_xy", blockNo), &blockNo, "grid point outside block")
		}
		idx := gridIndex(b.Grid, x, y)
		if req.Height != nil {
			b.Grid.Heights[idx] = *req.Height
		}
		if req.HeightDelta != nil {
			for i := range b.Grid.Heights {
				if req.GridX != nil && req.GridY != nil {
					if i == idx {
						b.Grid.Heights[i] += *req.HeightDelta
					}
				} else {
					b.Grid.Heights[i] += *req.HeightDelta
				}
			}
		}
		changes = append(changes, BlockChange{BlockNo: blockNo, Kind: ReasonHeight})
	}
	s.mu.Unlock()
	if len(changes) == 0 {
		changes = []BlockChange{{BlockNo: blockNo, Kind: ReasonHeight}}
	}
	return s.runLocked(ctx, changes, false, nil)
}

type ServiceState struct {
	Project        *Project                   `json:"project"`
	Manifest       *RunManifest               `json:"manifest"`
	Reports        map[string]*BoundaryReport `json:"reports"`
	FaultReports   []*AttributionReport       `json:"fault_reports"`
	Cache          map[int]*CacheEntry        `json:"cache"`
	Budget         BudgetOccupancy            `json:"budget"`
	Queue          []QueueEvent               `json:"queue"`
	RecoveryBlocks []int                      `json:"recovery_blocks"`
	VersionReset   bool                       `json:"version_reset"`
}

func (s *Service) State() ServiceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	cacheCopy := map[int]*CacheEntry{}
	for k, v := range s.cache {
		cacheCopy[k] = v
	}
	faultReports := make([]*AttributionReport, 0, len(s.proj.Truth.FaultCases))
	for _, fc := range s.proj.Truth.FaultCases {
		rep, _ := AttributeBoundary(fc)
		if rep != nil {
			faultReports = append(faultReports, rep)
		}
	}
	return ServiceState{Project: s.proj, Manifest: s.manifest, Reports: s.reports, FaultReports: faultReports,
		Cache: cacheCopy, Budget: s.meter.Occupancy(), Queue: s.meter.Events(),
		RecoveryBlocks: append([]int{}, s.recoveryBlocks...), VersionReset: s.versionReset}
}

func (s *Service) ExportJSON() ([]byte, error) {
	state := s.State()
	meshes := map[int]*Mesh{}
	for no, entry := range state.Cache {
		if entry != nil && entry.Mesh != nil {
			meshes[no] = entry.Mesh
		}
	}
	payload := struct {
		Project  *Project                   `json:"project"`
		Meshes   map[int]*Mesh              `json:"meshes"`
		Reports  map[string]*BoundaryReport `json:"boundary_reports"`
		Manifest *RunManifest               `json:"manifest"`
	}{state.Project, meshes, state.Reports, state.Manifest}
	return json.MarshalIndent(payload, "", "  ")
}

func WriteExport(path string, data []byte) error {
	if err := os.MkdirAll(filepathDir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func filepathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}
