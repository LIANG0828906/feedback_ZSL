package qos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type InjectionCase struct {
	Name         string         `json:"name"`
	Code         Code           `json:"code"`
	Path         string         `json:"path,omitempty"`
	Context      map[string]any `json:"context,omitempty"`
	Handled      bool           `json:"handled"`
	SourceStable bool           `json:"source_unchanged"`
	Detail       string         `json:"detail,omitempty"`
}

type InjectionReport struct {
	Cases   []InjectionCase `json:"cases"`
	AllPass bool            `json:"all_pass"`
}

func RunInjectionCases(ctx context.Context, seed int64, workDir string) (*InjectionReport, error) {
	project := Scenario(seed)
	project.SchemaVersion = 1
	v1, err := EncodeProject(project, 1)
	if err != nil {
		return nil, err
	}
	eng, err := NewEngine(project)
	if err != nil {
		return nil, err
	}
	result, err := eng.Run(ctx)
	if err != nil {
		return nil, err
	}
	truth, err := EncodeTruth(project, result)
	if err != nil {
		return nil, err
	}
	sourceV1 := append([]byte(nil), v1...)
	sourceTruth := append([]byte(nil), truth...)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, wrapError(EInternal, "inject", err, map[string]any{"dir": workDir})
	}
	report := &InjectionReport{AllPass: true}
	add := func(name string, err error, sourceStable bool, detail string) {
		c := InjectionCase{Name: name, Handled: err != nil, SourceStable: sourceStable, Detail: detail}
		if err != nil {
			c.Code = CodeOf(err)
			if c.Code == "" {
				c.Code = EInternal
			}
			var qe *Error
			if e, ok := err.(*Error); ok {
				qe = e
			}
			if qe != nil {
				c.Path = qe.Path
				c.Context = qe.Context
			}
		}
		report.Cases = append(report.Cases, c)
	}

	negative, err := mutateV1(v1, func(project map[string]any) error {
		ch := firstChannel(project)
		ch["rate_permille"] = -1
		return nil
	})
	if err != nil {
		return nil, err
	}
	_, loadErr := LoadProject(negative)
	add("negative_rate", loadErr, equalBytes(v1, sourceV1), "rate_permille=-1")

	small, err := mutateV1(v1, func(project map[string]any) error {
		ch := firstChannel(project)
		ch["burst_packets"] = 1
		ch["initial_watermark_packets"] = 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	_, loadErr = LoadProject(small)
	add("bucket_smaller_than_packet", loadErr, equalBytes(v1, sourceV1), "gold burst=1 packet, packet=320 bytes")

	conflict, err := mutateV1(v1, func(project map[string]any) error {
		ch := firstChannel(project)
		ch["rate_permyriad"] = 123
		return nil
	})
	if err != nil {
		return nil, err
	}
	_, loadErr = LoadProject(conflict)
	add("field_conflict", loadErr, equalBytes(v1, sourceV1), "rate_permille and conflicting rate_permyriad")

	threads, err := mutateV1(v1, func(project map[string]any) error {
		project["worker_count"] = 0
		return nil
	})
	if err != nil {
		return nil, err
	}
	_, loadErr = LoadProject(threads)
	add("thread_range", loadErr, equalBytes(v1, sourceV1), "worker_count=0")

	budget, err := mutateV1(v1, func(project map[string]any) error {
		project["egress_budget_packets"] = 0
		return nil
	})
	if err != nil {
		return nil, err
	}
	_, loadErr = LoadProject(budget)
	add("queue_budget_range", loadErr, equalBytes(v1, sourceV1), "egress_budget_packets=0")

	reversedTruth, err := reverseTruthQueue(truth)
	if err != nil {
		return nil, err
	}
	_, loadErr = DecodeTruth(reversedTruth, project)
	add("expiry_order_reversed", loadErr, equalBytes(truth, sourceTruth), "reverse one non-empty delay_queue")

	versionPath := filepath.Join(workDir, "version-mismatch.db")
	_ = os.Remove(versionPath)
	store, err := OpenCheckpoint(versionPath)
	if err != nil {
		return nil, err
	}
	snap, err := NewEngine(project)
	if err != nil {
		store.Close()
		return nil, err
	}
	if err := snap.StepUntil(3); err != nil {
		store.Close()
		return nil, err
	}
	if err := store.Save(ctx, project, snap.Snapshot()); err != nil {
		store.Close()
		return nil, err
	}
	store.Close()
	other := CloneProject(project)
	other.Version += "-different"
	store, err = OpenCheckpoint(versionPath)
	if err != nil {
		return nil, err
	}
	_, loadErr = store.Load(ctx, other)
	store.Close()
	add("checkpoint_version_mismatch", loadErr, equalBytes(v1, sourceV1), "checkpoint saved for gen-<seed>-r1, opened as different version")

	truncatedPath := filepath.Join(workDir, "truncated.db")
	_ = os.Remove(truncatedPath)
	_ = os.Remove(truncatedPath + "-wal")
	_ = os.Remove(truncatedPath + "-shm")
	store, err = OpenCheckpoint(truncatedPath)
	if err != nil {
		return nil, err
	}
	if err := store.Save(ctx, project, snap.Snapshot()); err != nil {
		store.Close()
		return nil, err
	}
	store.Close()
	if err := os.Truncate(truncatedPath, 80); err != nil {
		return nil, wrapError(EInternal, "inject.truncate", err, nil)
	}
	_, loadErr = OpenCheckpoint(truncatedPath)
	add("truncated_checkpoint", loadErr, equalBytes(v1, sourceV1), "SQLite file truncated to 80 bytes")

	for _, c := range report.Cases {
		if !c.Handled || c.Code == "" || !c.SourceStable {
			report.AllPass = false
			continue
		}
		expected := map[string]Code{
			"negative_rate":               ERateNegative,
			"bucket_smaller_than_packet":  EBucketTooSmall,
			"field_conflict":              EFieldConflict,
			"thread_range":                EThreadRange,
			"queue_budget_range":          EQueueBudget,
			"expiry_order_reversed":       EExpiryOrder,
			"checkpoint_version_mismatch": ECheckpointVersion,
			"truncated_checkpoint":        ECheckpointTruncated,
		}[c.Name]
		if c.Code != expected {
			report.AllPass = false
		}
	}
	return report, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mutateV1(raw []byte, mutate func(project map[string]any) error) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, wrapError(EInternal, "inject.mutate", err, nil)
	}
	project, ok := root["project"].(map[string]any)
	if !ok {
		return nil, NewError(EInternal, "inject.mutate", "project object missing", nil)
	}
	if err := mutate(project); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, wrapError(EInternal, "inject.mutate", err, nil)
	}
	return append(out, '\n'), nil
}

func firstChannel(project map[string]any) map[string]any {
	arr := project["shaping"].([]any)
	return arr[0].(map[string]any)
}

func reverseTruthQueue(raw []byte) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, wrapError(EInternal, "inject.truth", err, nil)
	}
	steps := root["steps"].([]any)
	for _, s := range steps {
		step := s.(map[string]any)
		q, _ := step["delay_queue"].([]any)
		if len(q) < 2 {
			continue
		}
		for i, j := 0, len(q)-1; i < j; i, j = i+1, j-1 {
			q[i], q[j] = q[j], q[i]
		}
		out, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return nil, wrapError(EInternal, "inject.truth", err, nil)
		}
		return append(out, '\n'), nil
	}
	return nil, NewError(EInternal, "inject.truth", fmt.Sprintf("no truth step has at least two delayed packets"), nil)
}
