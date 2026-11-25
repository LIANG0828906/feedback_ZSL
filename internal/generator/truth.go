package generator

import (
	"encoding/json"
	"os"
)

func LoadTruth(data []byte) (*TruthFile, error) {
	var truth TruthFile
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, err
	}
	return &truth, nil
}

func LoadTruthFile(path string) (*TruthFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadTruth(data)
}
