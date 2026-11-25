package scenarioio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"atelier/internal/model"
	"atelier/internal/validate"
)

func Write(path string, scenario model.Scenario) error {
	if err := validate.Scenario(&scenario); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(scenario, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Read(path string) (model.Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return model.Scenario{}, err
	}
	return Decode(b)
}

func Decode(b []byte) (model.Scenario, error) {
	var scenario model.Scenario
	if err := json.Unmarshal(b, &scenario); err != nil {
		return model.Scenario{}, fmt.Errorf("invalid scenario JSON: %w", err)
	}
	if err := validate.Scenario(&scenario); err != nil {
		return model.Scenario{}, err
	}
	return scenario, nil
}
