package macrokit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type WrittenSample struct {
	Slug  string
	Dir   string
	Files map[int]string
}

func WriteSampleSet(root string, set *SampleSet) ([]WrittenSample, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	seedData, _ := json.MarshalIndent(map[string]any{"seed": set.Seed}, "", "  ")
	if err := os.WriteFile(filepath.Join(root, "seed.json"), append(seedData, '\n'), 0o644); err != nil {
		return nil, err
	}

	written := make([]WrittenSample, 0, len(set.Projects))
	for _, sample := range set.Projects {
		dir := filepath.Join(root, sample.Slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		files := map[int]string{}
		names := map[int]string{
			FormatFlatFrameV1: "flat-frame-v1.json",
			FormatNestedMSV2:  "nested-ms-v2.json",
			FormatTreeFrameV3: "tree-frame-v3.json",
		}
		for _, formatNo := range []int{FormatFlatFrameV1, FormatNestedMSV2, FormatTreeFrameV3} {
			data, err := ExportProject(sample.Project, formatNo)
			if err != nil {
				return nil, fmt.Errorf("export %s format %d: %w", sample.Slug, formatNo, err)
			}
			path := filepath.Join(dir, names[formatNo])
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return nil, err
			}
			files[formatNo] = path
		}
		if err := writeJSONFile(filepath.Join(dir, "events.json"), sample.Events); err != nil {
			return nil, err
		}
		if err := writeJSONFile(filepath.Join(dir, "truth.json"), sample.Truth); err != nil {
			return nil, err
		}
		written = append(written, WrittenSample{Slug: sample.Slug, Dir: dir, Files: files})
	}
	return written, nil
}

func LoadEventStreamFile(path string) (EventStream, error) {
	var stream EventStream
	if err := readJSONFile(path, &stream); err != nil {
		return EventStream{}, &Error{Code: ESchema, Path: path, Message: "read event stream", Cause: err}
	}
	return stream, nil
}

func LoadTruthFile(path string) (*Truth, error) {
	var truth Truth
	if err := readJSONFile(path, &truth); err != nil {
		return nil, &Error{Code: ESchema, Path: path, Message: "read truth", Cause: err}
	}
	return &truth, nil
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
