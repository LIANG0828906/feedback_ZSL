package lightprobe

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
)

type RunOptions struct {
	Label              string     `json:"label"`
	Pool               PoolConfig `json:"pool"`
	CheckpointPath     string     `json:"checkpoint_path"`
	StopAfterCommits   int        `json:"stop_after_commits"`
	CancelAfterCommits int        `json:"cancel_after_commits"`
}

type PixelResult struct {
	PixelID   string     `json:"pixel_id"`
	X         int        `json:"x"`
	Y         int        `json:"y"`
	QueryID   string     `json:"query_id"`
	Radiance  [3]float64 `json:"radiance"`
	RGB       [3]uint8   `json:"rgb"`
	Uncertain bool       `json:"uncertain"`
	CacheKey  string     `json:"cache_key,omitempty"`
}

type PipelineResult struct {
	Label             string                  `json:"label"`
	CheckpointPath    string                  `json:"checkpoint_path,omitempty"`
	ProbeResults      map[string]ProbeResult  `json:"-"`
	QueryResults      []QueryResult           `json:"query_results"`
	Pixels            []PixelResult           `json:"pixels"`
	Graph             *DependencyGraph        `json:"graph,omitempty"`
	RoomStates        map[string]RoomProgress `json:"room_states"`
	RoomQueueStats    map[string]QueueStats   `json:"room_queue_stats"`
	Queue             QueueStats              `json:"queue"`
	CommitOrder       []string                `json:"commit_order"`
	RestoredRooms     []string                `json:"restored_rooms"`
	StaleRooms        []StaleRoom             `json:"stale_rooms"`
	RoomErrors        map[string]*Error       `json:"room_errors,omitempty"`
	StoppedAtBoundary bool                    `json:"stopped_at_boundary"`
	Store             *CheckpointStore        `json:"-"`
}

func RunPipeline(scene *Scene, opts RunOptions) (*PipelineResult, error) {
	return RunPipelineContext(context.Background(), scene, opts)
}

func RunPipelineContext(ctx context.Context, scene *Scene, opts RunOptions) (*PipelineResult, error) {
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	params := scene.Params
	poolCfg := opts.Pool
	if poolCfg.Workers <= 0 {
		poolCfg.Workers = params.Workers
	}
	if poolCfg.BufferLimit <= 0 {
		poolCfg.BufferLimit = params.BufferLimit
	}
	if poolCfg.QueueBudget <= 0 {
		poolCfg.QueueBudget = params.QueueBudget
	}

	out := &PipelineResult{
		Label: opts.Label, CheckpointPath: opts.CheckpointPath,
		ProbeResults:   map[string]ProbeResult{},
		RoomStates:     map[string]RoomProgress{},
		RoomQueueStats: map[string]QueueStats{},
		RoomErrors:     map[string]*Error{},
	}

	var store *CheckpointStore
	if opts.CheckpointPath != "" {
		var err error
		store, err = OpenCheckpoint(opts.CheckpointPath)
		if err != nil {
			return nil, err
		}
		out.Store = store
	}
	resVersions := map[string]string{}
	for _, r := range scene.Rooms {
		resVersions[r.ID] = r.ResolutionVersion
	}
	var valid map[string]RoomRecord
	if store != nil {
		valid, out.StaleRooms = store.VerifiedRooms(resVersions, params.ParamVersion)
	}

	roomIDs := make([]string, 0, len(scene.Rooms))
	for _, r := range scene.Rooms {
		roomIDs = append(roomIDs, r.ID)
	}
	sort.Strings(roomIDs)

	newCommits := 0
	for _, roomID := range roomIDs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		progress := NewRoomProgress(roomID)
		if rec, ok := valid[roomID]; ok {
			progress.State = StateCommitted
			progress.Progress = 1
			out.RoomStates[roomID] = progress
			out.RestoredRooms = append(out.RestoredRooms, roomID)
			for _, pc := range rec.Probes {
				rf := pc.RadianceFixed
				pr := ProbeResult{
					ProbeID: pc.ProbeID, RoomID: roomID, IJK: pc.IJK,
					RadianceFixed: rf, Truncations: pc.Truncations,
					Radiance: [3]float64{
						float64(rf[0]) / float64(params.FixedScale),
						float64(rf[1]) / float64(params.FixedScale),
						float64(rf[2]) / float64(params.FixedScale),
					},
					SHQuantResidualMax: pc.SHQuantResidualMax,
					CountRoundingError: pc.CountRoundingError,
					RawSampleCount:     pc.RawSampleCount,
					SampleCount:        pc.SampleCount,
					ContentHash:        pc.ContentHash,
				}
				out.ProbeResults[pc.ProbeID] = pr
			}
			continue
		}

		if err := progress.Transition(StateCollecting); err != nil {
			return nil, err
		}
		out.RoomStates[roomID] = progress
		probes := probesForRoom(scene, roomID)
		results, stats, err := RunProbePoolContext(ctx, probes, params, poolCfg)
		out.RoomQueueStats[roomID] = stats
		mergeQueueStats(&out.Queue, stats)
		if err != nil {
			de, _ := AsDomainError(err)
			if de == nil {
				de = domainError(ECancel, roomID, "pool", "%v", err)
			}
			_ = progress.Transition(StateFailed)
			progress.LastError = de
			out.RoomStates[roomID] = progress
			out.RoomErrors[roomID] = de
			continue
		}
		for id, pr := range results {
			out.ProbeResults[id] = pr
		}
		if err := progress.SetProgress(0.65); err != nil {
			return nil, err
		}
		if err := progress.Transition(StateReconstructed); err != nil {
			return nil, err
		}

		record := buildRoomRecord(scene, roomID, results, params)
		if store != nil {
			if err := store.CommitRoom(scene.Seed, record); err != nil {
				de, _ := AsDomainError(err)
				if de == nil {
					de = domainError(EDecode, roomID, "checkpoint", "%v", err)
				}
				_ = progress.Transition(StateFailed)
				progress.LastError = de
				out.RoomStates[roomID] = progress
				out.RoomErrors[roomID] = de
				continue
			}
		}
		if err := progress.Transition(StateCommitted); err != nil {
			return nil, err
		}
		out.RoomStates[roomID] = progress
		newCommits++
		if opts.CancelAfterCommits > 0 && newCommits >= opts.CancelAfterCommits {
			out.CommitOrder = currentCommitOrder(store, roomIDs, out.RoomStates)
			return out, context.Canceled
		}
		if opts.StopAfterCommits > 0 && newCommits >= opts.StopAfterCommits {
			out.StoppedAtBoundary = true
			out.CommitOrder = currentCommitOrder(store, roomIDs, out.RoomStates)
			return out, nil
		}
	}

	if len(out.RoomErrors) > 0 {
		out.CommitOrder = currentCommitOrder(store, roomIDs, out.RoomStates)
		return out, nil
	}

	queryResults, err := EvaluateQueries(scene, out.ProbeResults, params)
	if err != nil {
		return nil, err
	}
	out.QueryResults = queryResults
	queryByID := map[string]QueryResult{}
	for _, qr := range queryResults {
		queryByID[qr.Query.ID] = qr
	}
	for _, px := range scene.Pixels {
		qr := queryByID[px.QueryID]
		out.Pixels = append(out.Pixels, PixelResult{
			PixelID: px.ID, X: px.X, Y: px.Y, QueryID: px.QueryID,
			Radiance: qr.Radiance, RGB: ShadePixel(qr.Radiance), Uncertain: qr.Uncertain,
		})
	}
	out.Graph = BuildDependencyGraph(scene, queryResults)
	for i := range out.Pixels {
		if n := out.Graph.Nodes["pixel:"+out.Pixels[i].PixelID]; n != nil {
			out.Pixels[i].CacheKey = n.CacheKey
		}
	}
	for id, qr := range queryByID {
		if n := out.Graph.Nodes["query:"+id]; n != nil {
			qr.CacheKey = n.CacheKey
			queryByID[id] = qr
		}
	}
	out.CommitOrder = currentCommitOrder(store, roomIDs, out.RoomStates)
	finalizeQueueTotals(&out.Queue)
	return out, nil
}

func probesForRoom(scene *Scene, roomID string) []Probe {
	var out []Probe
	for _, p := range scene.Probes {
		if p.RoomID == roomID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return compareIJK(out[i].IJK, out[j].IJK) < 0 })
	return out
}

func compareIJK(a, b [3]int) int {
	for axis := 0; axis < 3; axis++ {
		if a[axis] < b[axis] {
			return -1
		}
		if a[axis] > b[axis] {
			return 1
		}
	}
	return 0
}

func buildRoomRecord(scene *Scene, roomID string, results map[string]ProbeResult, params Params) RoomRecord {
	room := scene.Room(roomID)
	probes := probesForRoom(scene, roomID)
	rec := RoomRecord{
		RoomID: roomID, ProbeDigests: map[string]string{},
		ResolutionVersion: room.ResolutionVersion,
		ParamVersion:      params.ParamVersion,
		AlgorithmVersion:  AlgorithmVersion,
	}
	var deps []string
	for _, p := range probes {
		r := results[p.ID]
		digest := p.ContentHash[len("probe-content:"):]
		rec.ProbeDigests[p.ID] = digest
		rec.Probes = append(rec.Probes, ProbeCheckpoint{
			ProbeID: p.ID, IJK: p.IJK, Digest: digest, ContentHash: p.ContentHash,
			RadianceFixed: r.RadianceFixed, Truncations: r.Truncations,
			RawSampleCount: r.RawSampleCount, SampleCount: r.SampleCount,
			SHQuantResidualMax: r.SHQuantResidualMax, CountRoundingError: r.CountRoundingError,
		})
		rec.RadianceBuffer = append(rec.RadianceBuffer, r.RadianceFixed)
		deps = append(deps, "radiance:"+p.ID, "sh:"+p.ID, "recon:"+p.ID)
	}
	sort.Strings(deps)
	rec.DependencySet = deps
	rec.ContentHash = ComputeRoomHash(rec)
	return rec
}

func currentCommitOrder(store *CheckpointStore, roomIDs []string, states map[string]RoomProgress) []string {
	if store != nil {
		snap := store.Snapshot()
		if len(snap.Order) > 0 {
			return append([]string(nil), snap.Order...)
		}
	}
	var order []string
	for _, id := range roomIDs {
		if st, ok := states[id]; ok && st.State == StateCommitted {
			order = append(order, id)
		}
	}
	return order
}

func mergeQueueStats(total *QueueStats, add QueueStats) {
	total.Workers = add.Workers
	total.BufferLimit = add.BufferLimit
	total.QueueBudget = add.QueueBudget
	total.ProbeCount += add.ProbeCount
	total.Submitted += add.Submitted
	total.Completed += add.Completed
	if add.PeakActiveWorkers > total.PeakActiveWorkers {
		total.PeakActiveWorkers = add.PeakActiveWorkers
	}
	if add.PeakQueued > total.PeakQueued {
		total.PeakQueued = add.PeakQueued
	}
	if add.PeakBufferObserved > total.PeakBufferObserved {
		total.PeakBufferObserved = add.PeakBufferObserved
	}
	total.WaitCount += add.WaitCount
	total.TotalWaitNS += add.TotalWaitNS
	total.Waits = append(total.Waits, add.Waits...)
}

func finalizeQueueTotals(q *QueueStats) {
	q.TotalWaitMS = float64(q.TotalWaitNS) / 1e6
}

func ShadePixel(radiance [3]float64) [3]uint8 {
	var rgb [3]uint8
	for c := 0; c < 3; c++ {
		v := math.Max(0, math.Min(1, radiance[c]))
		mapped := 1 - math.Exp(-1.35*v)
		gamma := math.Pow(mapped, 1/2.2)
		rgb[c] = uint8(math.Round(gamma * 255))
	}
	return rgb
}

func DiffPipelines(want, got *PipelineResult) *Error {
	if stringsJoin(want.CommitOrder) != stringsJoin(got.CommitOrder) {
		return domainError(EDiff, "pipeline", "commit_order",
			"commit order mismatch want %v got %v", want.CommitOrder, got.CommitOrder)
	}
	roomIDs := make([]string, 0, len(want.RoomStates))
	for id := range want.RoomStates {
		roomIDs = append(roomIDs, id)
	}
	sort.Strings(roomIDs)
	for _, roomID := range roomIDs {
		var wanted []ProbeResult
		var actual []ProbeResult
		for _, r := range want.ProbeResults {
			if r.RoomID == roomID {
				wanted = append(wanted, r)
			}
		}
		for _, r := range got.ProbeResults {
			if r.RoomID == roomID {
				actual = append(actual, r)
			}
		}
		sort.Slice(wanted, func(i, j int) bool { return compareIJK(wanted[i].IJK, wanted[j].IJK) < 0 })
		sort.Slice(actual, func(i, j int) bool { return compareIJK(actual[i].IJK, actual[j].IJK) < 0 })
		for i := 0; i < len(wanted) || i < len(actual); i++ {
			if i >= len(actual) {
				return diffProbe(roomID, wanted[i].IJK, "missing_probe", "missing reconstructed probe %s", wanted[i].ProbeID)
			}
			if i >= len(wanted) {
				return diffProbe(roomID, actual[i].IJK, "unexpected_probe", "unexpected reconstructed probe %s", actual[i].ProbeID)
			}
			if wanted[i].ProbeID != actual[i].ProbeID || compareIJK(wanted[i].IJK, actual[i].IJK) != 0 {
				return diffProbe(roomID, wanted[i].IJK, "probe_id", "probe order mismatch want %s got %s", wanted[i].ProbeID, actual[i].ProbeID)
			}
			for ch := 0; ch < 3; ch++ {
				if wanted[i].RadianceFixed[ch] != actual[i].RadianceFixed[ch] {
					return diffProbe(roomID, wanted[i].IJK,
						"radiance_fixed["+channelName(ch)+"]",
						"channel %s want %d got %d", channelName(ch), wanted[i].RadianceFixed[ch], actual[i].RadianceFixed[ch])
				}
			}
		}
	}

	wantQ := map[string]QueryResult{}
	for _, q := range want.QueryResults {
		wantQ[q.Query.ID] = q
	}
	for _, gq := range got.QueryResults {
		wq, ok := wantQ[gq.Query.ID]
		if !ok {
			return domainError(EDiff, gq.Query.PrimaryRoomID, "query:"+gq.Query.ID, "unexpected query")
		}
		for ch := 0; ch < 3; ch++ {
			if wq.RadianceFixed[ch] != gq.RadianceFixed[ch] {
				return domainError(EDiff, gq.Query.PrimaryRoomID, "query:"+gq.Query.ID+":radiance_fixed["+channelName(ch)+"]",
					"query %s channel %s want %d got %d", gq.Query.ID, channelName(ch), wq.RadianceFixed[ch], gq.RadianceFixed[ch])
			}
		}
		if wq.Uncertain != gq.Uncertain {
			return domainError(EDiff, gq.Query.PrimaryRoomID, "query:"+gq.Query.ID+":uncertain",
				"want %v got %v", wq.Uncertain, gq.Uncertain)
		}
	}

	wantPx := map[string]PixelResult{}
	for _, p := range want.Pixels {
		wantPx[p.PixelID] = p
	}
	for _, gp := range got.Pixels {
		wp, ok := wantPx[gp.PixelID]
		if !ok {
			return domainError(EDiff, "screen", "pixel:"+gp.PixelID, "unexpected pixel")
		}
		if wp.RGB != gp.RGB {
			return domainError(EDiff, "screen", "pixel:"+gp.PixelID+":rgb", "pixel color want %v got %v", wp.RGB, gp.RGB)
		}
		if wp.Uncertain != gp.Uncertain {
			return domainError(EDiff, "screen", "pixel:"+gp.PixelID+":uncertain", "want %v got %v", wp.Uncertain, gp.Uncertain)
		}
	}
	return nil
}

func diffProbe(roomID string, ijk [3]int, field, format string, args ...any) *Error {
	coord := "grid_coord=[" + itoa(ijk[0]) + "," + itoa(ijk[1]) + "," + itoa(ijk[2]) + "]"
	msg := coord + " " + field + ": " + fmt.Sprintf(format, args...)
	return domainError(EDiff, roomID, coord+"."+field, "%s", msg)
}

func channelName(ch int) string {
	return []string{"R", "G", "B"}[ch]
}

func stringsJoin(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += "|"
		}
		out += s
	}
	return out
}

func CheckErrorBounds(pr *PipelineResult) (maxActual, maxUpper float64, violations []string) {
	for _, qr := range pr.QueryResults {
		maxActual = math.Max(maxActual, qr.Report.ActualErrorMax)
		maxUpper = math.Max(maxUpper, qr.Report.AccumulatedUpper)
		if qr.Report.ActualErrorMax > qr.Report.AccumulatedUpper+1e-10 {
			violations = append(violations, qr.Query.ID)
		}
		for ch, cr := range qr.Report.Channels {
			truth := cr.ExpectedRadiance
			if truth < cr.LowerBound-1e-10 || truth > cr.UpperBound+1e-10 {
				violations = append(violations, qr.Query.ID+":bound-"+channelName(ch))
			}
		}
	}
	return
}

func acceptanceDir(base string) string {
	if base == "" {
		return ".probebench"
	}
	return filepath.Join(base)
}
