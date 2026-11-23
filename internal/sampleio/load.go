package sampleio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"inputkernel/internal/model"
)

type Set struct {
	Root      string
	Manifest  *model.Manifest
	Scenarios []*model.Scenario
}

func Load(root string) (*Set, error) {
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return nil, model.Schema("manifest.json", "", err.Error())
	}
	manifest, err := model.DecodeManifest(raw)
	if err != nil {
		return nil, err
	}
	set := &Set{Root: root, Manifest: manifest}
	for _, entry := range manifest.Scenarios {
		if filepath.IsAbs(entry.File) || strings.Contains(entry.File, "..") {
			return nil, model.Schema("manifest.json.scenarios", entry.File, "scenario path must stay inside the sample directory")
		}
		data, err := os.ReadFile(filepath.Join(root, entry.File))
		if err != nil {
			return nil, model.Schema("manifest.json.scenarios", entry.File, err.Error())
		}
		scenario, err := model.DecodeScenario(data)
		if err != nil {
			return nil, err
		}
		if scenario.ID != entry.ID {
			return nil, model.Schema("manifest.json.scenarios", entry.ID, fmt.Sprintf("manifest id does not match scenario id %s", scenario.ID))
		}
		if scenario.Seed != manifest.Seed {
			return nil, model.Schema("scenario.seed", scenario.Seed, fmt.Sprintf("scenario seed does not match manifest seed %d", manifest.Seed))
		}
		set.Scenarios = append(set.Scenarios, scenario)
	}
	return set, nil
}

func EventHash(events []model.Event) string {
	data, _ := json.Marshal(struct {
		Events []model.Event `json:"events"`
	}{Events: events})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ConfigHash(s *model.Scenario) string {
	data, _ := json.Marshal(struct {
		SchemaVersion         int             `json:"schema_version"`
		FixedPrecisionVersion string          `json:"fixed_precision_version"`
		BindingVersion        string          `json:"binding_version"`
		Devices               []model.Device  `json:"devices"`
		InitialDeviceID       string          `json:"initial_device_id"`
		Curves                []model.Curve   `json:"curves"`
		Bindings              []model.Binding `json:"bindings"`
	}{
		SchemaVersion:         s.SchemaVersion,
		FixedPrecisionVersion: s.FixedPrecisionVersion,
		BindingVersion:        s.BindingVersion,
		Devices:               s.Devices,
		InitialDeviceID:       s.InitialDeviceID,
		Curves:                s.Curves,
		Bindings:              s.Bindings,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func Clone(s *model.Scenario) *model.Scenario {
	data, _ := json.Marshal(s)
	var out model.Scenario
	_ = json.Unmarshal(data, &out)
	return &out
}
