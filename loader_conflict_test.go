package chaosdiag_test

import (
	"encoding/json"
	"strings"
	"testing"

	"chaosdiag/gen"
	"chaosdiag/incremental"
	"chaosdiag/loader"
	"chaosdiag/model"
	"chaosdiag/workbench"
)

func TestLoaderReturnsStableConflictFields(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(bundle.Docs[model.SchemaV1], &doc); err != nil {
		t.Fatal(err)
	}
	commands := doc["fault_commands"].([]any)
	for _, raw := range commands {
		cmd := raw.(map[string]any)
		if cmd["command_id"] == "I-DELAY-INV" {
			cmd["latency_ticks"] = float64(999)
		}
	}
	bad, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loader.Load(bad)
	ce, ok := model.AsError(err)
	if !ok || ce.Code != model.CodeFieldConflict || !strings.Contains(ce.Field, "latency") {
		t.Fatalf("err=%v want %s on latency field", err, model.CodeFieldConflict)
	}

	_, err = loader.LoadVersion("chaosdiag.io/v9", []byte(`{}`))
	ce, ok = model.AsError(err)
	if !ok || ce.Code != model.CodeSchemaUnsupported {
		t.Fatalf("schema err=%v want %s", err, model.CodeSchemaUnsupported)
	}
}

func TestSourceDocumentsRemainStableAfterEdits(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	beforeV3 := append([]byte{}, bundle.Docs[model.SchemaV3]...)
	svc, err := workbench.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	delay := int64(123)
	_, err = svc.Edit("v3", incremental.Mutation{Kind: "instruction_amplitude", InstructionID: "I-DELAY-INV", DelayMS: &delay})
	if err != nil {
		t.Fatal(err)
	}
	after, err := svc.Document("v3")
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(beforeV3) {
		t.Fatal("source schema document changed after workbench edit")
	}
}
