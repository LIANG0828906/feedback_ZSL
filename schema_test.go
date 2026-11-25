package thermalcache

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestThreeSchemasLoadToOneModel(t *testing.T) {
	f := fixture42(t)
	versions := []string{SchemaV1, SchemaV2, SchemaV3}
	var first *Project
	for _, version := range versions {
		p := loadVersion(t, f, version)
		if p.SchemaVersion != version {
			t.Fatalf("%s schema version normalized to %s", version, p.SchemaVersion)
		}
		if !p.SemanticEqual(f.Canonical) {
			t.Fatalf("%s does not equal canonical model", version)
		}
		if first == nil {
			first = p
			continue
		}
		if !first.SemanticEqual(p) {
			t.Fatalf("%s fingerprint differs from %s", version, first.SchemaVersion)
		}
		if len(first.Nodes) != len(p.Nodes) || len(first.Objects) != len(p.Objects) {
			t.Fatalf("%s set sizes differ", version)
		}
		for i := range first.Nodes {
			if first.Nodes[i].ID != p.Nodes[i].ID || first.Nodes[i].Tier != p.Nodes[i].Tier {
				t.Fatalf("%s node traversal order differs at %d", version, i)
			}
			if first.Nodes[i].CapacityBytes != p.Nodes[i].CapacityBytes {
				t.Fatalf("%s node capacity normalization differs", version)
			}
		}
		for i := range first.Objects {
			if first.Objects[i].ID != p.Objects[i].ID || first.Objects[i].InitialTier != p.Objects[i].InitialTier {
				t.Fatalf("%s object traversal order differs at %d", version, i)
			}
			if first.Objects[i].LastAccessMs != p.Objects[i].LastAccessMs {
				t.Fatalf("%s time normalization differs for %s", version, p.Objects[i].ID)
			}
		}
	}
	if got := loadVersion(t, f, SchemaV1).Nodes[0].CapacityBytes; got != 12288 {
		t.Fatalf("v1 byte capacity not preserved: %d", got)
	}
	if got := loadVersion(t, f, SchemaV2).Nodes[0].CapacityBytes; got != 12288 {
		t.Fatalf("v2 KiB capacity not normalized: %d", got)
	}
	if got := loadVersion(t, f, SchemaV3).Objects[8].TTLMs; got != 300 {
		t.Fatalf("v3 millisecond TTL not preserved: %d", got)
	}
	for _, version := range versions {
		obj := loadVersion(t, f, version).Objects[4]
		if obj.Watermark == nil || *obj.Watermark != 3500 {
			t.Fatalf("%s optional heat watermark not normalized: %+v", version, obj.Watermark)
		}
	}
}

func TestUnknownFieldCombinationIsStableCode(t *testing.T) {
	f := fixture42(t)
	var doc map[string]any
	if err := json.Unmarshal(f.Documents[SchemaV1], &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema_version"] = "normalized/v2"
	bad, _ := json.Marshal(doc)
	_, err := LoadProject(bad)
	assertCode(t, err, CodeSchemaUnknownField)

	doc = map[string]any{}
	_ = json.Unmarshal(f.Documents[SchemaV2], &doc)
	doc["unexpected_field"] = 1
	bad, _ = json.Marshal(doc)
	_, err = LoadProject(bad)
	assertCode(t, err, CodeSchemaUnknownField)
}

func TestLoaderDoesNotMutateSourceBytes(t *testing.T) {
	f := fixture42(t)
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		before := append([]byte(nil), f.Documents[version]...)
		if _, err := LoadProject(f.Documents[version]); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, f.Documents[version]) {
			t.Fatalf("%s loader modified input bytes", version)
		}
	}
}
