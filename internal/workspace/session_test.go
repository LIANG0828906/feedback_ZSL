package workspace_test

import (
	"context"
	"testing"

	"economycore/internal/core"
	"economycore/internal/samples"
	"economycore/internal/workspace"
)

func newManager(t *testing.T) *workspace.Manager {
	t.Helper()
	dir := t.TempDir()
	if _, err := samples.Generate(dir, 5, []samples.Profile{{
		ID: "txn", Intents: 160, Horizon: 320, TruthSamples: 6,
	}}); err != nil {
		t.Fatal(err)
	}
	return workspace.NewManager(dir)
}

func TestBatchFailureRollsBackAllEdits(t *testing.T) {
	m := newManager(t)
	session, err := m.Open("txn")
	if err != nil {
		t.Fatal(err)
	}
	before := session.View()
	nodeID := before.Nodes[0].ID
	valid := workspace.Edit{Kind: "node_workers", NodeID: nodeID, Workers: intPtr(0)}
	invalid := workspace.Edit{Kind: "node_workers", NodeID: nodeID, Workers: intPtr(9999)}
	_, err = session.Commit(context.Background(), workspace.Batch{
		BatchID: "batch-bad",
		Edits:   []workspace.Edit{valid, invalid},
	})
	if core.CodeOf(err) != core.CodeTxn {
		t.Fatalf("code=%s want E_TXN: %v", core.CodeOf(err), err)
	}
	e := core.AsError(err)
	if e.BatchID != "batch-bad" {
		t.Fatalf("batch id=%q", e.BatchID)
	}
	after := session.View()
	if after.SceneHash != before.SceneHash || after.Revision != before.Revision || after.Nodes[0].Workers != before.Nodes[0].Workers {
		t.Fatalf("failed batch polluted committed state: hash %s->%s revision %d->%d workers %d->%d",
			before.SceneHash, after.SceneHash, before.Revision, after.Revision,
			before.Nodes[0].Workers, after.Nodes[0].Workers)
	}
}

func TestValidCommitChangesRevisionAndWindows(t *testing.T) {
	m := newManager(t)
	session, err := m.Open("txn")
	if err != nil {
		t.Fatal(err)
	}
	before := session.View()
	nodeID := before.Nodes[0].ID
	view, err := session.Commit(context.Background(), workspace.Batch{
		BatchID: "batch-ok",
		Edits: []workspace.Edit{{
			Kind: "node_workers", NodeID: nodeID, Workers: intPtr(0),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Version != before.Version+1 || view.Revision <= before.Revision {
		t.Fatalf("version/revision not advanced: before=%d/%d after=%d/%d",
			before.Version, before.Revision, view.Version, view.Revision)
	}
	if view.SceneHash == before.SceneHash {
		t.Fatal("scene hash did not change")
	}
	if view.Exact == nil || len(view.Exact.Windows) == 0 {
		t.Fatal("commit did not run windowed settlement")
	}
	if view.Heatmap.Equal != true || view.Heatmap.MaxAbs != 0 {
		t.Fatalf("preview/exact heatmap diverged: %+v", view.Heatmap)
	}
}

func TestApplyDraftThenDiscard(t *testing.T) {
	m := newManager(t)
	session, err := m.Open("txn")
	if err != nil {
		t.Fatal(err)
	}
	before := session.View()
	nodeID := before.Nodes[0].ID
	draft, err := session.ApplyDraft(workspace.Batch{
		BatchID: "draft-1",
		Edits: []workspace.Edit{{
			Kind: "node_params", NodeID: nodeID,
			Outputs:  mapPtr(map[string]int64{"grain": 1234}),
			Inputs:   mapPtr(map[string]int64{}),
			Capacity: mapPtr(map[string]int64{"grain": 5000}),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !draft.Dirty || draft.SceneHash == before.SceneHash {
		t.Fatalf("draft did not become dirty: %+v", draft)
	}
	discarded := session.Discard()
	if discarded.SceneHash != before.SceneHash || discarded.Dirty || discarded.Revision != before.Revision {
		t.Fatalf("discard did not restore committed copy: %+v", discarded)
	}
}

func TestPreviewDraftDoesNotMutateWorkingCopy(t *testing.T) {
	m := newManager(t)
	session, err := m.Open("txn")
	if err != nil {
		t.Fatal(err)
	}
	before := session.View()
	preview, err := session.PreviewDraft(context.Background(), workspace.Batch{
		BatchID: "preview-only",
		Edits: []workspace.Edit{{
			Kind: "node_workers", NodeID: before.Nodes[0].ID, Workers: intPtr(0),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview == nil || preview.Final.Tick != before.FinalTick {
		t.Fatalf("bad preview: %+v", preview)
	}
	after := session.View()
	if after.SceneHash != before.SceneHash || after.Revision != before.Revision {
		t.Fatal("preview mutated the working copy")
	}
}

func intPtr(v int) *int { return &v }

func mapPtr(v map[string]int64) *map[string]int64 { return &v }
