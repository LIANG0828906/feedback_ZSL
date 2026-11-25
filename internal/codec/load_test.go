package codec_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"skeletonretarget/internal/codec"
	"skeletonretarget/internal/fixed"
	"skeletonretarget/internal/generator"
	"skeletonretarget/internal/model"
)

func loadThree(t *testing.T) []*model.Document {
	t.Helper()
	sample, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	var docs []*model.Document
	for _, data := range [][]byte{sample.Studio, sample.Engine, sample.Legacy} {
		doc, err := codec.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, doc)
	}
	return docs
}

func TestThreeFormatsLoadToEquivalentInternalModel(t *testing.T) {
	docs := loadThree(t)
	hash := docs[0].ContentHash()
	for _, doc := range docs {
		if doc.ContentHash() != hash {
			t.Fatalf("action hash differs: %s vs %s", doc.ContentHash(), hash)
		}
		if len(doc.Clip.Frames) != generator.FrameCount {
			t.Fatalf("frame count %d", len(doc.Clip.Frames))
		}
		if len(doc.Skeleton.Joints) != 6 || len(doc.Target.Joints) != 6 {
			t.Fatalf("joint counts differ")
		}
		for i := 1; i < len(doc.Clip.Frames); i++ {
			if doc.Clip.Frames[i].Index <= doc.Clip.Frames[i-1].Index {
				t.Fatal("frames are not ordered")
			}
		}
	}
	for i := range docs[0].Skeleton.Joints {
		a, b := docs[0].Skeleton.Joints[i], docs[1].Skeleton.Joints[i]
		if a.ID != b.ID || a.Parent != b.Parent {
			t.Fatalf("topology mismatch at %d", i)
		}
		if a.Constraint != b.Constraint {
			t.Fatalf("constraint mismatch at %d: %+v vs %+v", i, a.Constraint, b.Constraint)
		}
		if drift := fixed.VecDeltaPPM(a.Rest, b.Rest); drift > 1 {
			t.Fatalf("rest drift %dppm at %d", drift, i)
		}
	}
	for i := range docs[0].Clip.Frames {
		for j := range docs[0].Clip.Frames[i].Rotations {
			a, b := docs[0].Clip.Frames[i].Rotations[j], docs[2].Clip.Frames[i].Rotations[j]
			if drift := fixed.QuatDeltaPPM(a, b); drift > 1 {
				t.Fatalf("rotation drift %dppm at frame %d joint %d", drift, i, j)
			}
		}
	}
	for i := range docs[0].Target.Joints {
		a, b := docs[0].Target.Joints[i], docs[2].Target.Joints[i]
		if a.ID != b.ID || a.Parent != b.Parent || a.Proportion != b.Proportion {
			t.Fatalf("target mismatch at %d", i)
		}
		if drift := fixed.VecDeltaPPM(a.Rest, b.Rest); drift > 1 {
			t.Fatalf("target rest drift %dppm at %d", drift, i)
		}
	}
	for _, doc := range docs {
		if len(doc.Conversions) == 0 {
			t.Fatal("expected conversion/truncation records")
		}
	}
}

func TestSchemaVersionAndMissingFields(t *testing.T) {
	sample, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(sample.Studio), `"version": "3"`, `"version": "99"`, 1)
	_, err = codec.Load([]byte(unknown))
	assertCode(t, err, model.CodeSchema)
	var appErr *model.Error
	if !errors.As(err, &appErr) || appErr.Path != "$.version" || !strings.Contains(appErr.Version, "99") {
		t.Fatalf("schema error lacks path/version: %+v", appErr)
	}
	missing := strings.Replace(string(sample.Studio), `"sourceVersion": "studio-3",`, ``, 1)
	_, err = codec.Load([]byte(missing))
	assertCode(t, err, model.CodeSchema)
}

func TestDecodeCompOrderTopoAndMapErrors(t *testing.T) {
	sample, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Load(sample.Studio[:len(sample.Studio)/2]); !model.IsCode(err, model.CodeDecode) {
		t.Fatalf("expected E_DECODE, got %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(sample.Studio, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc["skeleton"].(map[string]any)["tree"].([]any)[0].(map[string]any)
	root["rest"] = []any{float64(0), float64(0), float64(0)}
	clip := doc["clip"].(map[string]any)
	frames := clip["frames"].([]any)
	pose := frames[0].(map[string]any)["poses"].([]any)[0].(map[string]any)
	pose["q"] = []any{float64(2_000_000), float64(0), float64(0), float64(0)}
	broken, _ := json.Marshal(doc)
	if _, err := codec.Load(broken); !model.IsCode(err, model.CodeComp) {
		t.Fatalf("expected E_COMP, got %v", err)
	} else if appErr := asAppError(err); appErr.Joint == "" || appErr.Frame != 0 {
		t.Fatalf("E_COMP lacks joint/frame: %+v", appErr)
	}

	var ordered map[string]any
	_ = json.Unmarshal(sample.Studio, &ordered)
	studioFrames := ordered["clip"].(map[string]any)["frames"].([]any)
	studioFrames[0], studioFrames[1] = studioFrames[1], studioFrames[0]
	broken, _ = json.Marshal(ordered)
	if _, err := codec.Load(broken); !model.IsCode(err, model.CodeOrder) {
		t.Fatalf("expected E_ORDER, got %v", err)
	}

	var topo map[string]any
	_ = json.Unmarshal(sample.Engine, &topo)
	spine := topo["skeleton"].(map[string]any)["bones"].([]any)[1].(map[string]any)
	spine["parent"] = "missing"
	broken, _ = json.Marshal(topo)
	if _, err := codec.Load(broken); !model.IsCode(err, model.CodeTopo) {
		t.Fatalf("expected E_TOPO, got %v", err)
	}

	var mapped map[string]any
	_ = json.Unmarshal(sample.Engine, &mapped)
	mappings := mapped["retarget"].(map[string]any)["mappings"].([]any)
	mappings[0].(map[string]any)["target"] = "missing_target"
	broken, _ = json.Marshal(mapped)
	if _, err := codec.Load(broken); !model.IsCode(err, model.CodeMap) {
		t.Fatalf("expected E_MAP, got %v", err)
	}
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if !model.IsCode(err, code) {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func asAppError(err error) *model.Error {
	var appErr *model.Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return &model.Error{}
}
