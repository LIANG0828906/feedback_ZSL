package branches_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/liang/hapticfeedback/internal/haptic/branches"
	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
	"github.com/liang/hapticfeedback/internal/haptic/samplegen"
)

func TestThreeBranchesSemanticEquivalenceAndTraces(t *testing.T) {
	canonical := samplegen.CanonicalStream()
	loaded := map[int]*model.Stream{}
	for branch := 1; branch <= 3; branch++ {
		exported, err := branches.Export(canonical, branch)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := branches.Load(exported.Data)
		if err != nil {
			t.Fatalf("branch %d load: %v", branch, err)
		}
		loaded[branch] = stream
		if stream.SourceBranch != branch {
			t.Fatalf("source branch=%d want %d", stream.SourceBranch, branch)
		}
		if len(stream.Events[0].Traces) < 6 {
			t.Fatalf("semantic traces missing: %d", len(stream.Events[0].Traces))
		}
	}
	diff := branches.CompareStreams(loaded[1], loaded[2])
	if !diff.Equal {
		t.Fatalf("branch 1/2 diff at %s: %#v vs %#v", diff.SemanticName, diff.Left, diff.Right)
	}
	diff = branches.CompareStreams(loaded[1], loaded[3])
	if !diff.Equal {
		t.Fatalf("branch 1/3 diff at %s: %s", diff.SemanticName, diff.Message)
	}
	if loaded[1].Fingerprint() != loaded[2].Fingerprint() || loaded[1].Fingerprint() != loaded[3].Fingerprint() {
		t.Fatal("semantic fingerprints differ")
	}
}

func TestVersionAndRequiredPathErrors(t *testing.T) {
	raw := map[string]any{"branch": float64(404), "stream_id": "x", "seed": 1, "frames": []any{}}
	data, _ := json.Marshal(raw)
	_, err := branches.Load(data)
	if errs.CodeOf(err) != errs.EVersion {
		t.Fatalf("unknown branch code=%v err=%v", errs.CodeOf(err), err)
	}
	if he, _ := errs.As(err); he.Path != "$.branch" {
		t.Fatalf("path=%q", he.Path)
	}

	exported, _ := branches.Export(samplegen.CanonicalStream(), 1)
	var file map[string]any
	_ = json.Unmarshal(exported.Data, &file)
	delete(file, "stream_id")
	data, _ = json.Marshal(file)
	_, err = branches.Load(data)
	if errs.CodeOf(err) != errs.EVersion {
		t.Fatalf("missing stream_id code=%v", errs.CodeOf(err))
	}
	if he, _ := errs.As(err); he.Path != "$.stream_id" {
		t.Fatalf("path=%q", he.Path)
	}
}

func TestTimeConversionOverflow(t *testing.T) {
	exported, _ := branches.Export(samplegen.CanonicalStream(), 1)
	var file map[string]any
	_ = json.Unmarshal(exported.Data, &file)
	frames := file["frames"].([]any)
	frame := frames[0].(map[string]any)
	frame["at"] = math.MaxInt64
	data, _ := json.Marshal(file)
	_, err := branches.Load(data)
	if errs.CodeOf(err) != errs.EOver {
		t.Fatalf("code=%v err=%v", errs.CodeOf(err), err)
	}
	he, _ := errs.As(err)
	if he.Field != "at" || he.FrameNo != 1 {
		t.Fatalf("overflow context field=%s frame=%d details=%v", he.Field, he.FrameNo, he.Details)
	}
	if he.Details["side_a"] == nil || he.Details["side_b"] == nil {
		t.Fatalf("overflow must include both side values: %v", he.Details)
	}
}

func TestMissingFlagHasFrame(t *testing.T) {
	data, err := samplegen.InjectedBranchBytes(samplegen.InjectMissingFlag, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = branches.Load(data)
	if errs.CodeOf(err) != errs.EFlag {
		t.Fatalf("code=%v err=%v", errs.CodeOf(err), err)
	}
	he, _ := errs.As(err)
	if he.FrameNo != 6 || he.Channel != "ch_left" {
		t.Fatalf("flag context frame=%d channel=%q", he.FrameNo, he.Channel)
	}
}

func TestLossyPriorityClampRoundtripComparison(t *testing.T) {
	stream := samplegen.CanonicalStream()
	stream.Events[0].Priority = 99
	result, err := branches.Export(stream, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Losses {
		if item.InternalName == "priority" && item.Disposition == "clamp_to_9" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected priority clamp loss, got %#v", result.Losses)
	}
	reloaded, err := branches.Load(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	diff := branches.CompareStreams(&stream, reloaded)
	if diff.Equal || diff.SemanticName != "priority" {
		t.Fatalf("expected first diff priority, got %#v", diff)
	}
}
