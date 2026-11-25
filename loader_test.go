package orchestrator

import "testing"

func TestMillisecondEventMustAlignToLogicalTick(t *testing.T) {
	document := []byte(`{
		"schema_version":"cluster.v1",
		"project_id":"p",
		"snapshot_id":"s",
		"revision":"r",
		"clock":{"tick_millis_num":5,"tick_millis_den":2},
		"timeout_bits":32,
		"nodes":[{"id":"a","rack":"r","zone_id_abs":1,"workloads":[]}],
		"events":[{"at_ms":1,"seq":1,"kind":"probe","node_id":"a"}]
	}`)
	_, err := LoadProject(document)
	if CodeOf(err) != CodeTimeNotAligned {
		t.Fatalf("code = %s, want %s (%v)", CodeOf(err), CodeTimeNotAligned, err)
	}
}

func TestDeclaredTimeoutWidthIsEnforced(t *testing.T) {
	document := []byte(`{
		"schema_version":"cluster.v1",
		"project_id":"p",
		"snapshot_id":"s",
		"revision":"r",
		"clock":{"tick_millis_num":1,"tick_millis_den":1},
		"timeout_bits":32,
		"nodes":[{
			"id":"a",
			"rack":"r",
			"zone_id_abs":1,
			"isolation_timeout_ms":4294967296,
			"workloads":[]
		}],
		"events":[{"at_ms":0,"seq":1,"kind":"probe","node_id":"a"}]
	}`)
	_, err := LoadProject(document)
	if CodeOf(err) != CodeTimeoutOverflow {
		t.Fatalf("code = %s, want %s (%v)", CodeOf(err), CodeTimeoutOverflow, err)
	}
}
