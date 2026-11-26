package workbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"

	"enginelab.local/internal/checkpoint"
	"enginelab.local/internal/loader"
	"enginelab.local/internal/model"
	"enginelab.local/internal/recovery"
)

type InjectRequest struct {
	Kind        string              `json:"kind"`
	Schema      model.SchemaVersion `json:"schema,omitempty"`
	SegmentID   string              `json:"segment_id,omitempty"`
	Workers     int                 `json:"workers,omitempty"`
	QueueDepth  int                 `json:"queue_depth,omitempty"`
	MemoryBytes int64               `json:"memory_bytes,omitempty"`
	CutRecords  int                 `json:"cut_records,omitempty"`
	Incomplete  bool                `json:"incomplete,omitempty"`
}

type InjectResponse struct {
	OK          bool                  `json:"ok"`
	Recoverable bool                  `json:"recoverable,omitempty"`
	Kind        string                `json:"kind"`
	Code        model.ErrorCode       `json:"code,omitempty"`
	Error       *model.Error          `json:"error,omitempty"`
	Diagnostics []model.Diagnostic    `json:"diagnostics,omitempty"`
	Message     string                `json:"message,omitempty"`
	Context     map[string]any        `json:"context,omitempty"`
	Result      *model.RecoveryResult `json:"result,omitempty"`
	Located     any                   `json:"loaded,omitempty"`
}

func errResponse(kind string, err error) *InjectResponse {
	be := model.AsError(err)
	return &InjectResponse{OK: false, Kind: kind, Code: be.Code, Error: be, Message: be.Message}
}

func (s *Service) Inject(ctx context.Context, req InjectRequest) *InjectResponse {
	switch req.Kind {
	case "duplicate_segment_ref":
		return s.injectDuplicateRef(req)
	case "cross_version_base_conflict":
		return s.injectBaseConflict(req)
	case "duplicate_record":
		return s.injectDuplicateRecord(req)
	case "sequence_order":
		return s.injectOrder(req)
	case "dependency_cycle":
		return s.injectCycle(req)
	case "dependency_missing":
		return s.injectMissing(req)
	case "truncated_segment_file", "incomplete_record":
		return s.injectCorruptSegment(req)
	case "truncated_checkpoint":
		return s.injectTruncatedCheckpoint(req)
	case "budget_threads":
		return s.injectThreadBudget(req)
	case "budget_memory":
		return s.injectMemoryBudget(req)
	case "backpressure":
		return s.injectBackpressure(req)
	case "schema_version_mismatch", "project_version_mismatch":
		return s.injectSchemaVersionMismatch(req)
	case "version_mismatch", "version_mismatch_snapshot":
		return s.injectVersionMismatch(ctx, req)
	default:
		return &InjectResponse{OK: false, Kind: req.Kind, Code: model.EStateConflict, Error: model.NewError(model.EStateConflict, "$.kind", "unknown injection kind %q", req.Kind)}
	}
}

func (s *Service) injectDuplicateRef(req InjectRequest) *InjectResponse {
	var doc map[string]any
	if err := json.Unmarshal(s.bundle.Docs[model.SchemaV1], &doc); err != nil {
		return errResponse(req.Kind, err)
	}
	segments, _ := doc["segments"].([]any)
	for _, raw := range segments {
		seg, _ := raw.(map[string]any)
		if seg["segment_id"] == "seg-001" {
			refs, _ := seg["predecessor_ids"].([]any)
			seg["predecessor_ids"] = append(refs, "seg-000")
		}
	}
	data, _ := json.Marshal(doc)
	_, err := loader.LoadDocument(data)
	if err == nil {
		err = model.NewError(model.EStateConflict, "seg-001.predecessor_ids", "injection did not fail")
	}
	return errResponse(req.Kind, err)
}

func (s *Service) injectBaseConflict(req InjectRequest) *InjectResponse {
	var doc map[string]any
	if err := json.Unmarshal(s.bundle.Docs[model.SchemaV2], &doc); err != nil {
		return errResponse(req.Kind, err)
	}
	for _, raw := range doc["segments"].([]any) {
		seg := raw.(map[string]any)
		if seg["id"] == "seg-001" {
			seg["baseLsn"] = float64(999999)
		}
	}
	data, _ := json.Marshal(doc)
	bad, err := loader.LoadDocument(data, loader.LoadOptions{ExpectedSchema: model.SchemaV2})
	if err != nil {
		return errResponse(req.Kind, err)
	}
	v1, err := loader.LoadDocument(s.bundle.Docs[model.SchemaV1])
	if err != nil {
		return errResponse(req.Kind, err)
	}
	v3, err := loader.LoadDocument(s.bundle.Docs[model.SchemaV3])
	if err != nil {
		return errResponse(req.Kind, err)
	}
	err = recovery.CompareVersionProjects(map[model.SchemaVersion]*model.Project{
		model.SchemaV1: v1, model.SchemaV2: bad, model.SchemaV3: v3,
	})
	return errResponse(req.Kind, err)
}

func (s *Service) injectDuplicateRecord(req InjectRequest) *InjectResponse {
	p, err := recovery.CloneProject(s.bundle.Project)
	if err != nil {
		return errResponse(req.Kind, err)
	}
	src := p.Segment("seg-000").Records[0]
	dup := *src
	target := p.Segment("seg-001")
	dup.SegmentID = target.ID
	target.Records = append(target.Records, &dup)
	return errResponse(req.Kind, recovery.ValidateSequence(p))
}

func (s *Service) injectOrder(req InjectRequest) *InjectResponse {
	p, err := recovery.CloneProject(s.bundle.Project)
	if err != nil {
		return errResponse(req.Kind, err)
	}
	seg := p.Segment("seg-003")
	seg.Records[0], seg.Records[1] = seg.Records[1], seg.Records[0]
	return errResponse(req.Kind, recovery.ValidateSequence(p))
}

func (s *Service) injectCycle(req InjectRequest) *InjectResponse {
	p, err := recovery.CloneProject(s.bundle.Project)
	if err != nil {
		return errResponse(req.Kind, err)
	}
	p.Segment("seg-000").PredecessorIDs = []string{"seg-005"}
	return errResponse(req.Kind, recovery.ValidateGraph(p))
}

func (s *Service) injectMissing(req InjectRequest) *InjectResponse {
	p, err := recovery.CloneProject(s.bundle.Project)
	if err != nil {
		return errResponse(req.Kind, err)
	}
	p.Segment("seg-001").PredecessorIDs = []string{"seg-404"}
	return errResponse(req.Kind, recovery.ValidateGraph(p))
}

func (s *Service) injectCorruptSegment(req InjectRequest) *InjectResponse {
	segmentID := req.SegmentID
	if segmentID == "" {
		segmentID = "seg-003"
	}
	raw := s.bundle.SegmentFiles[segmentID]
	if raw == nil {
		return errResponse(req.Kind, model.NewError(model.EFieldRequired, "segment_id", "no segment file %q", segmentID))
	}
	var corrupted []byte
	code := model.ESegmentTruncated
	var err error
	if req.Kind == "incomplete_record" || req.Incomplete {
		corrupted = loader.CorruptLastRecordByte(raw)
		code = model.ERecordIncomplete
	} else {
		count := req.CutRecords
		if count <= 0 {
			count = 6
		}
		corrupted, err = loader.TruncateSegmentFileToRecords(raw, count)
		if err != nil {
			return errResponse(req.Kind, err)
		}
	}
	parsed, perr := loader.ParseSegmentFile(segmentID, corrupted)
	if perr != nil {
		return errResponse(req.Kind, perr)
	}
	p, err := recovery.CloneProject(s.bundle.Project)
	if err != nil {
		return errResponse(req.Kind, err)
	}
	target := p.Segment(segmentID)
	target.Records = parsed.Records
	target.Integrity = parsed.Integrity
	target.Diagnostics = parsed.Diagnostics
	target.CompleteRecords = parsed.CompleteRecords
	target.RawRecordCount = parsed.RawRecordCount
	if len(parsed.Records) > 0 {
		target.LastLSN = parsed.Records[len(parsed.Records)-1].LSN
	}
	result, rerr := recovery.RecoverOneShot(p, recovery.Options{Budget: s.cfg.Budget})
	if rerr != nil {
		return errResponse(req.Kind, rerr)
	}
	matchesTruth := reflect.DeepEqual(result.Items, s.bundle.Truth.Items)
	be := model.NewError(code, segmentID, "recoverable segment corruption; only complete records were replayed")
	resp := &InjectResponse{
		OK:          true,
		Recoverable: true,
		Kind:        req.Kind,
		Code:        code,
		Error:       be,
		Diagnostics: parsed.Diagnostics,
		Message:     be.Message,
		Result:      result,
		Context: map[string]any{
			"parsed_integrity":      parsed.Integrity,
			"complete_records":      parsed.CompleteRecords,
			"raw_record_count":      parsed.RawRecordCount,
			"items_match_truth":     matchesTruth,
			"last_recovered_lsn":    result.SegmentMap[segmentID].LastRecovered.LSN,
			"last_recovered_record": result.SegmentMap[segmentID].LastRecovered.RecordID,
		},
	}
	return resp
}

func (s *Service) injectTruncatedCheckpoint(req InjectRequest) *InjectResponse {
	raw := append([]byte(nil), s.bundle.CheckpointFile...)
	cut := len(raw) / 2
	if req.CutRecords > 0 && req.CutRecords < len(raw) {
		cut = req.CutRecords
	}
	_, perr := loader.ParseCheckpointFile(raw[:cut])
	if perr == nil {
		perr = model.NewError(model.ECPCorrupt, "$.checkpoint_file", "truncated checkpoint unexpectedly parsed")
	}
	pe := model.AsError(perr)
	p, err := recovery.CloneProject(s.bundle.Project)
	if err != nil {
		return errResponse(req.Kind, err)
	}
	p.Checkpoints = nil
	p.LatestCheckpoint = nil
	result, rerr := recovery.RecoverOneShot(p, recovery.Options{Budget: s.cfg.Budget, BaselineItems: map[string]model.DataItem{}})
	if rerr != nil {
		return errResponse(req.Kind, rerr)
	}
	return &InjectResponse{
		OK:          true,
		Recoverable: true,
		Kind:        req.Kind,
		Code:        model.ECPCorrupt,
		Error:       pe,
		Message:     "checkpoint file was truncated; ignored and replayed from the declared start",
		Result:      result,
		Context:     map[string]any{"parse_error": pe, "all_segments_replayed": allRangesReplay(result)},
	}
}

func allRangesReplay(r *model.RecoveryResult) bool {
	for _, rng := range r.Ranges {
		if !rng.Replay {
			return false
		}
	}
	return true
}

func (s *Service) injectThreadBudget(req InjectRequest) *InjectResponse {
	workers := req.Workers
	if workers == 0 {
		workers = recovery.HardMaxWorkers + 1
	}
	_, err := recovery.NormalizeBudget(model.Budget{Workers: workers, QueueDepth: req.QueueDepth, MaxSegmentMemoryBytes: recovery.DefaultMaxSegmentMemoryBytes})
	return errResponse(req.Kind, err)
}

func (s *Service) injectMemoryBudget(req InjectRequest) *InjectResponse {
	mem := req.MemoryBytes
	if mem == 0 {
		mem = recovery.HardMaxSegmentMemoryBytes + 1
	}
	_, err := recovery.NormalizeBudget(model.Budget{Workers: 4, MaxSegmentMemoryBytes: mem})
	return errResponse(req.Kind, err)
}

func (s *Service) injectBackpressure(req InjectRequest) *InjectResponse {
	budget := model.Budget{Workers: 1, QueueDepth: 0, MaxSegmentMemoryBytes: recovery.DefaultMaxSegmentMemoryBytes}
	if req.Workers > 0 {
		budget.Workers = req.Workers
	}
	if req.QueueDepth > 0 {
		budget.QueueDepth = req.QueueDepth
	}
	result, err := recovery.RecoverOneShot(s.bundle.Project, recovery.Options{Budget: budget, BaselineItems: s.bundle.Truth.CheckpointItems})
	resp := errResponse(req.Kind, err)
	if result != nil {
		resp.OK = false
		resp.Result = result
		resp.Diagnostics = result.Backpressure
		resp.Context = map[string]any{"admission_capacity": budget.Workers + budget.QueueDepth}
	}
	return resp
}

func (s *Service) injectVersionMismatch(ctx context.Context, req InjectRequest) *InjectResponse {
	path := filepath.Join(os.TempDir(), "enginelab-version-mismatch.sqlite")
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	store, err := checkpoint.Open(path)
	if err != nil {
		return errResponse(req.Kind, err)
	}
	defer store.Close()
	_, err = store.SaveRecovery(ctx, checkpoint.SaveRequest{
		CheckpointID:       "cp-bad-version",
		ProjectID:          s.bundle.Project.ID,
		SchemaVersion:      "recovery-checkpoint-v0",
		GlobalCommittedLSN: 1,
	})
	if err != nil {
		return errResponse(req.Kind, err)
	}
	_, lerr := store.LoadCommitted(ctx, model.CheckpointSchemaVersion)
	if lerr == nil {
		return errResponse(req.Kind, model.NewError(model.EStateConflict, "$.version", "version mismatch was not detected"))
	}
	be := model.AsError(lerr)
	p, err := recovery.CloneProject(s.bundle.Project)
	if err != nil {
		return errResponse(req.Kind, err)
	}
	p.Checkpoints = nil
	p.LatestCheckpoint = nil
	full, ferr := recovery.RecoverOneShot(p, recovery.Options{Budget: s.cfg.Budget, BaselineItems: map[string]model.DataItem{}})
	if ferr != nil {
		return errResponse(req.Kind, ferr)
	}
	return &InjectResponse{
		OK:          false,
		Recoverable: true,
		Kind:        req.Kind,
		Code:        be.Code,
		Error:       be,
		Message:     "checkpoint version mismatch; replayed from the declared start",
		Result:      full,
		Context:     map[string]any{"full_replay_from_start": true, "all_segments_replayed": allRangesReplay(full)},
	}
}

func (s *Service) injectSchemaVersionMismatch(req InjectRequest) *InjectResponse {
	_, err := loader.LoadDocument(s.bundle.Docs[model.SchemaV1], loader.LoadOptions{ExpectedSchema: model.SchemaV3})
	if err == nil {
		err = model.NewError(model.ESchemaVersion, "$.schema", "schema mismatch was not detected")
	}
	return errResponse(req.Kind, err)
}
