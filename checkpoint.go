package planopt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

type CheckpointManifest struct {
	FormatVersion  string `json:"format_version"`
	CommittedSeq   int    `json:"committed_seq"`
	CommittedPlans []int  `json:"committed_plans"`
	Cache          Cache  `json:"cache"`
}

type PlanStateEnvelope struct {
	FormatVersion string    `json:"format_version"`
	PlanSeq       int       `json:"plan_seq"`
	State         PlanState `json:"state"`
	SHA256        string    `json:"sha256"`
}

type PlanState struct {
	PlanSeq    int            `json:"plan_seq"`
	PlanID     string         `json:"plan_id"`
	Normalized NormalizedPlan `json:"normalized"`
	Optimized  OptimizedPlan  `json:"optimized"`
}

type RecoveredCheckpoint struct {
	Manifest CheckpointManifest
	States   []PlanState
}

// RecoverOrRebuild implements the required version policy: a matching
// checkpoint is used, while a version mismatch causes whole-query
// reoptimization and a new committed checkpoint. Truncation remains an
// observable E_CHECKPOINT_TRUNCATED error and is not silently discarded.
func (c *FileCheckpoint) RecoverOrRebuild(project Project, bindings BindingSet) (results []OptimizedPlan, rebuilt bool, err error) {
	recovered, recoverErr := c.Recover()
	if recoverErr == nil {
		results = make([]OptimizedPlan, 0, len(recovered.States))
		for _, state := range recovered.States {
			results = append(results, state.Optimized)
		}
		sort.Slice(results, func(i, j int) bool { return results[i].PlanSeq < results[j].PlanSeq })
		return results, false, nil
	}
	business, ok := recoverErr.(*Error)
	if !ok || business.Code != CodeCheckpointVersionMismatch {
		return nil, false, recoverErr
	}
	engine, err := NewIncrementalEngine(project, bindings)
	if err != nil {
		return nil, false, err
	}
	cache := engine.CacheSnapshot()
	for _, plan := range engine.Results() {
		if err := c.CommitPlan(plan, cache); err != nil {
			return nil, false, err
		}
	}
	return engine.Results(), true, nil
}

type FileCheckpoint struct {
	Dir string
}

func NewFileCheckpoint(dir string) *FileCheckpoint {
	return &FileCheckpoint{Dir: dir}
}

func (c *FileCheckpoint) paths() (manifest, planDir string) {
	return filepath.Join(c.Dir, "checkpoint-manifest.json"), filepath.Join(c.Dir, "plans")
}

// WritePlanState persists one plan atomically without advancing the global
// committed sequence. CommitPlan calls this before manifest replacement.
func (c *FileCheckpoint) WritePlanState(opt OptimizedPlan) error {
	_, planDir := c.paths()
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		return Errorf(CodeInvariant, "checkpoint.mkdir", "%v", err)
	}
	state := PlanState{PlanSeq: opt.PlanSeq, PlanID: opt.PlanID, Normalized: opt.Normalized, Optimized: opt}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return Errorf(CodeInvariant, "checkpoint.marshal_state", "%v", err)
	}
	sum := sha256.Sum256(stateJSON)
	envelope := PlanStateEnvelope{
		FormatVersion: CheckpointFormatVersion, PlanSeq: opt.PlanSeq,
		State: state, SHA256: hex.EncodeToString(sum[:]),
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return Errorf(CodeInvariant, "checkpoint.marshal_envelope", "%v", err)
	}
	path := filepath.Join(planDir, planStateFileName(opt.PlanSeq))
	if err := atomicWriteFile(path, data); err != nil {
		return Errorf(CodeInvariant, "checkpoint.write_state", "%v", err)
	}
	return nil
}

// CommitPlan follows the required order: plan state first, global manifest/seq
// second. A process kill between the two leaves only an ignored orphan state.
func (c *FileCheckpoint) CommitPlan(opt OptimizedPlan, cache Cache) error {
	if err := c.WritePlanState(opt); err != nil {
		return err
	}
	manifestPath, _ := c.paths()
	manifest := CheckpointManifest{}
	if b, err := os.ReadFile(manifestPath); err == nil {
		_ = json.Unmarshal(b, &manifest)
	}
	if manifest.FormatVersion != CheckpointFormatVersion {
		manifest.FormatVersion = CheckpointFormatVersion
		manifest.CommittedSeq = 0
		manifest.CommittedPlans = nil
	}
	if manifest.CommittedSeq < opt.PlanSeq {
		manifest.CommittedSeq = opt.PlanSeq
	}
	if !intSliceContains(manifest.CommittedPlans, opt.PlanSeq) {
		manifest.CommittedPlans = append(manifest.CommittedPlans, opt.PlanSeq)
	}
	manifest.Cache = cache
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Errorf(CodeInvariant, "checkpoint.marshal_manifest", "%v", err)
	}
	if err := atomicWriteFile(manifestPath, data); err != nil {
		return Errorf(CodeInvariant, "checkpoint.commit_manifest", "%v", err)
	}
	return nil
}

func (c *FileCheckpoint) Recover() (RecoveredCheckpoint, error) {
	manifestPath, planDir := c.paths()
	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return RecoveredCheckpoint{Manifest: CheckpointManifest{FormatVersion: CheckpointFormatVersion}}, nil
	}
	if err != nil {
		return RecoveredCheckpoint{}, Errorf(CodeCheckpointTruncated, "checkpoint.read_manifest", "%v", err)
	}
	var manifest CheckpointManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return RecoveredCheckpoint{}, Errorf(CodeCheckpointTruncated, "checkpoint.parse_manifest", "%v", err)
	}
	if manifest.FormatVersion != CheckpointFormatVersion {
		return RecoveredCheckpoint{}, Errorf(CodeCheckpointVersionMismatch, "checkpoint.version", "snapshot=%q supported=%q", manifest.FormatVersion, CheckpointFormatVersion)
	}
	out := RecoveredCheckpoint{Manifest: manifest}
	for _, seq := range manifest.CommittedPlans {
		if seq > manifest.CommittedSeq {
			continue
		}
		path := filepath.Join(planDir, planStateFileName(seq))
		raw, err := os.ReadFile(path)
		if err != nil {
			return RecoveredCheckpoint{}, Errorf(CodeCheckpointTruncated, "checkpoint.read_plan", "plan %d: %v", seq, err)
		}
		var envelope PlanStateEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return RecoveredCheckpoint{}, Errorf(CodeCheckpointTruncated, "checkpoint.parse_plan", "plan %d: %v", seq, err)
		}
		stateJSON, err := json.Marshal(envelope.State)
		if err != nil {
			return RecoveredCheckpoint{}, Errorf(CodeCheckpointTruncated, "checkpoint.rehash_plan", "plan %d: %v", seq, err)
		}
		sum := sha256.Sum256(stateJSON)
		if envelope.SHA256 != hex.EncodeToString(sum[:]) || envelope.FormatVersion != CheckpointFormatVersion || envelope.PlanSeq != seq {
			return RecoveredCheckpoint{}, Errorf(CodeCheckpointTruncated, "checkpoint.checksum_plan", "plan %d checksum/version/sequence mismatch", seq)
		}
		out.States = append(out.States, envelope.State)
	}
	return out, nil
}

func (c *FileCheckpoint) InjectTruncatedManifest() error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	path, _ := c.paths()
	valid, _ := json.Marshal(CheckpointManifest{FormatVersion: CheckpointFormatVersion, CommittedSeq: 6})
	if len(valid) < 8 {
		valid = []byte(`{"format_version":"planopt-checkpoint/v1","committed_seq":6}`)
	}
	return os.WriteFile(path, valid[:len(valid)/2], 0o644)
}

func (c *FileCheckpoint) InjectVersionMismatchSnapshot() error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	path, _ := c.paths()
	old := CheckpointManifest{FormatVersion: "planopt-checkpoint/v0", CommittedSeq: 6}
	data, _ := json.MarshalIndent(old, "", "  ")
	return atomicWriteFile(path, data)
}

func planStateFileName(seq int) string {
	return "plan-" + leftPad4(seq) + ".json"
}

func leftPad4(seq int) string {
	s := ""
	if seq < 10 {
		s = "000"
	} else if seq < 100 {
		s = "00"
	} else if seq < 1000 {
		s = "0"
	}
	return s + itoa(seq)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func intSliceContains(in []int, v int) bool {
	for _, x := range in {
		if x == v {
			return true
		}
	}
	return false
}

func atomicWriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
