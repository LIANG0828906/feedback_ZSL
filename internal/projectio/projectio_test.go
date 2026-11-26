package projectio

import (
	"strings"
	"testing"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/generator"
	"mirrorcast/internal/session"
)

func TestThreeSchemaWritersAndLoadersMatch(t *testing.T) {
	suite, err := generator.GenerateSuite(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, generated := range suite.Projects {
		var loaded []*session.Project
		for _, version := range []string{session.SchemaV1, session.SchemaV2, session.SchemaV3} {
			data, err := Write(generated.Project, version)
			if err != nil {
				t.Fatalf("%s write %s: %v", generated.Name, version, err)
			}
			project, err := LoadVersion(data, version)
			if err != nil {
				t.Fatalf("%s load %s: %v", generated.Name, version, err)
			}
			loaded = append(loaded, project)
		}
		if mismatch := projectMismatchForTest(loaded[0], loaded[1]); mismatch != "" {
			t.Fatalf("v1/v2 mismatch: %s", mismatch)
		}
		if mismatch := projectMismatchForTest(loaded[0], loaded[2]); mismatch != "" {
			t.Fatalf("v1/v3 mismatch: %s", mismatch)
		}
	}
}

func TestV3ResolutionConflictStableCode(t *testing.T) {
	jsonData := `{
      "schemaVersion":"` + SchemaV3Wire + `",
      "sessionId":"conflict",
      "captureRegion":{"widthPixels":320,"heightPixels":180},
      "deliveryBudget":{"totalKbps":100000,"workerThreads":2,"bufferUnits":1048576},
      "topologyGroups":[{"groupId":"h1","displays":[{
        "displayId":"s1","widthPixels":320,"heightPixels":200,
        "equivalentAreaKiloPixels":999,
        "scalePermyriad":10000,"minimumScalePermyriad":2500,
        "chroma":{"planarFormat":"PLANAR_420","derivedBitsPerPixel":12}
      }]}]
    }`
	_, err := Load([]byte(jsonData))
	if bizerror.CodeOf(err) != bizerror.EResolutionConflict {
		t.Fatalf("code=%q err=%v", bizerror.CodeOf(err), err)
	}
}

func TestV3ChromaSemanticConflictStableCode(t *testing.T) {
	jsonData := `{
      "schemaVersion":"` + SchemaV3Wire + `",
      "sessionId":"chroma-conflict",
      "captureRegion":{"widthPixels":320,"heightPixels":180},
      "deliveryBudget":{"totalKbps":100000,"workerThreads":2,"bufferUnits":1048576},
      "topologyGroups":[{"groupId":"h1","displays":[{
        "displayId":"s1","widthPixels":320,"heightPixels":200,
        "scalePermyriad":10000,"minimumScalePermyriad":2500,
        "chroma":{"planarFormat":"PLANAR_420","derivedBitsPerPixel":24}
      }]}]
    }`
	_, err := Load([]byte(jsonData))
	if bizerror.CodeOf(err) != bizerror.ESchemaConflict {
		t.Fatalf("code=%q err=%v", bizerror.CodeOf(err), err)
	}
	if err != nil && !strings.Contains(err.Error(), "planar enum and derived bit count") {
		t.Fatalf("unexpected conflict detail: %v", err)
	}
}

func TestUnsupportedSchemaStableCode(t *testing.T) {
	_, err := Load([]byte(`{"schema":"mirrorcast.project/v9"}`))
	if bizerror.CodeOf(err) != bizerror.ESchemaUnsupported {
		t.Fatalf("code=%q", bizerror.CodeOf(err))
	}
}

func projectMismatchForTest(a, b *session.Project) string {
	if len(a.Screens) != len(b.Screens) {
		return "screen count"
	}
	for i := range a.Screens {
		if a.Screens[i] != b.Screens[i] {
			return "screen mismatch at " + a.Screens[i].ID
		}
	}
	return ""
}
