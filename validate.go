package colortrace

import (
	"encoding/json"
	"fmt"
	"math"
)

// LoadProject parses and validates a deterministic texture project.
func LoadProject(data []byte) (*Project, error) {
	if err := ValidateSchemaJSON(data); err != nil {
		return nil, err
	}
	var project Project
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, schemaError("$", "invalid JSON: %v", err)
	}
	if err := ValidateProject(&project); err != nil {
		return nil, err
	}
	return &project, nil
}

// ValidateSchemaJSON checks presence and broad JSON shape before typed loading.
func ValidateSchemaJSON(data []byte) error {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return schemaError("$", "invalid JSON: %v", err)
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return schemaError("$", "root must be an object")
	}
	if err := requireKeys(obj, "$", "schema_version", "project_id", "seed", "base_bits", "width", "height", "chunk_size", "planes", "stages"); err != nil {
		return err
	}
	planes, err := requirePathArray(obj, "$.planes", true)
	if err != nil {
		return err
	}
	for i, item := range planes {
		path := fmt.Sprintf("$.planes[%d]", i)
		plane, err := asObject(item, path)
		if err != nil {
			return err
		}
		if err := requireKeys(plane, path, "name", "width", "height", "values"); err != nil {
			return err
		}
		if _, err := requirePathArray(plane, path+".values", false); err != nil {
			return err
		}
	}
	stages, err := requirePathArray(obj, "$.stages", true)
	if err != nil {
		return err
	}
	for i, item := range stages {
		path := fmt.Sprintf("$.stages[%d]", i)
		stage, err := asObject(item, path)
		if err != nil {
			return err
		}
		if err := requireKeys(stage, path, "index", "id", "factor_x", "factor_y", "accum_scale", "bits", "kernel", "param_version"); err != nil {
			return err
		}
		kernel, err := requirePathObject(stage, path+".kernel", "name", "radius_x", "radius_y", "norm_scale", "taps")
		if err != nil {
			return err
		}
		if _, err := requirePathArray(kernel, path+".kernel.taps", true); err != nil {
			return err
		}
	}
	return nil
}

// ValidateProject checks semantic compatibility and declared parameter ranges.
func ValidateProject(project *Project) error {
	if project == nil {
		return schemaError("$", "project is null")
	}
	if project.SchemaVersion != SchemaVersion {
		return schemaError("$.schema_version", "must be %q", SchemaVersion)
	}
	if project.ProjectID == "" {
		return schemaError("$.project_id", "is empty")
	}
	if project.BaseBits < MinQuantBits || project.BaseBits > MaxSourceBits {
		return paraError(-1, MinQuantBits, MaxSourceBits, "base_bits %d out of supported range", project.BaseBits)
	}
	if project.Width <= 0 || project.Height <= 0 {
		return schemaError("$.width/height", "dimensions must be positive")
	}
	if project.ChunkSize <= 0 {
		return schemaError("$.chunk_size", "must be positive")
	}
	if project.DominantShareThreshold <= 0 || project.DominantShareThreshold > 1 {
		return schemaError("$.dominant_share_threshold", "must be in (0,1]")
	}
	if len(project.Planes) == 0 {
		return schemaError("$.planes", "at least one plane is required")
	}
	names := map[string]bool{}
	dims := map[string][2]int{}
	for i, plane := range project.Planes {
		path := fmt.Sprintf("$.planes[%d]", i)
		if plane.Name == "" {
			return schemaError(path+".name", "is empty")
		}
		if names[plane.Name] {
			return schemaError(path+".name", "duplicate plane %q", plane.Name)
		}
		names[plane.Name] = true
		if plane.Width <= 0 || plane.Height <= 0 {
			return schemaError(path+".width/height", "plane dimensions must be positive")
		}
		if len(plane.Values) != plane.Width*plane.Height {
			return &Error{
				Code: EDecode,
				Path: path + ".values",
				Message: fmt.Sprintf("truncated plane %q: expected %d samples, got %d",
					plane.Name, plane.Width*plane.Height, len(plane.Values)),
			}
		}
		for _, value := range plane.Values {
			if value < 0 || value >= 1<<project.BaseBits {
				return schemaError(path+".values", "value %d outside base bit depth", value)
			}
		}
		dims[plane.Name] = [2]int{plane.Width, plane.Height}
	}
	if len(project.Stages) == 0 {
		return schemaError("$.stages", "at least one stage is required")
	}
	prevBits := project.BaseBits
	for i, stage := range project.Stages {
		if stage.Index != i {
			return schemaError(fmt.Sprintf("$.stages[%d].index", i), "stage indices must be contiguous from 0; got %d", stage.Index)
		}
		if stage.ID == "" {
			return schemaError(fmt.Sprintf("$.stages[%d].id", i), "is empty")
		}
		if stage.FactorX <= 0 || stage.FactorY <= 0 {
			return schemaError(fmt.Sprintf("$.stages[%d].factor_x/factor_y", i), "downsample factors must be positive")
		}
		if stage.AccumScale <= 0 {
			return schemaError(fmt.Sprintf("$.stages[%d].accum_scale", i), "must be positive")
		}
		maxBits := MaxQuantBits
		if prevBits < maxBits {
			maxBits = prevBits
		}
		if stage.Bits < MinQuantBits || stage.Bits > maxBits {
			return paraError(i, MinQuantBits, maxBits,
				"quantization bits %d outside supported truncation range", stage.Bits)
		}
		if err := validateKernel(stage.Kernel, fmt.Sprintf("$.stages[%d].kernel", i)); err != nil {
			return err
		}
		if stage.ChromaKernel != nil {
			if err := validateKernel(*stage.ChromaKernel, fmt.Sprintf("$.stages[%d].chroma_kernel", i)); err != nil {
				return err
			}
		}
		for _, plane := range project.Planes {
			current := dims[plane.Name]
			if current[0]%stage.FactorX != 0 || current[1]%stage.FactorY != 0 {
				return schemaError(fmt.Sprintf("$.stages[%d].factor_x/factor_y", i),
					"plane %q dimensions %dx%d are not divisible by %dx%d",
					plane.Name, current[0], current[1], stage.FactorX, stage.FactorY)
			}
			dims[plane.Name] = [2]int{current[0] / stage.FactorX, current[1] / stage.FactorY}
		}
		prevBits = stage.Bits
	}
	targetNames := map[string]bool{}
	for i, target := range project.Targets {
		path := fmt.Sprintf("$.targets[%d]", i)
		if target == nil || target.Name == "" {
			return schemaError(path+".name", "target plane name is missing")
		}
		if targetNames[target.Name] {
			return schemaError(path+".name", "duplicate target plane %q", target.Name)
		}
		targetNames[target.Name] = true
		expectedDims := dims[target.Name]
		if expectedDims[0] == 0 {
			return schemaError(path+".name", "target references unknown plane %q", target.Name)
		}
		if target.Width != expectedDims[0] || target.Height != expectedDims[1] {
			return schemaError(path, "target %q dimensions %dx%d incompatible with declared factors; expected %dx%d",
				target.Name, target.Width, target.Height, expectedDims[0], expectedDims[1])
		}
		if len(target.Values) != target.Width*target.Height {
			return &Error{
				Code: EDecode, Path: path + ".values",
				Message: fmt.Sprintf("truncated target %q: expected %d samples, got %d",
					target.Name, target.Width*target.Height, len(target.Values)),
			}
		}
		if target.Bits != 0 && target.Bits != prevBits {
			return schemaError(path+".bits", "target bits %d does not match final stage bits %d", target.Bits, prevBits)
		}
		maxCode := int64(1)<<uint(prevBits) - 1
		for _, value := range target.Values {
			if value < 0 || value > maxCode {
				return schemaError(path+".values", "target code %d outside final %d-bit range", value, prevBits)
			}
		}
	}
	for _, query := range project.TracePoints {
		plane := project.Plane(query.Plane)
		if plane == nil {
			return schemaError("$.trace_points", "unknown plane %q", query.Plane)
		}
		if query.X < 0 || query.X >= plane.Width || query.Y < 0 || query.Y >= plane.Height {
			return schemaError("$.trace_points", "coord (%d,%d) outside plane %q", query.X, query.Y, query.Plane)
		}
	}
	return nil
}

const MaxSourceBits = 16

func validateKernel(kernel Kernel, path string) error {
	if kernel.Name == "" {
		return schemaError(path+".name", "is empty")
	}
	if kernel.NormScale <= 0 {
		return schemaError(path+".norm_scale", "must be positive")
	}
	if len(kernel.Taps) == 0 {
		return schemaError(path+".taps", "at least one tap is required")
	}
	if kernel.RadiusX < 0 || kernel.RadiusY < 0 {
		return schemaError(path+".radius_x/radius_y", "radii must be non-negative")
	}
	for i, tap := range kernel.Taps {
		tapPath := fmt.Sprintf("%s.taps[%d]", path, i)
		if tap.W <= 0 {
			return schemaError(tapPath+".w", "tap weight must be positive")
		}
		if int(math.Abs(float64(tap.DX))) > kernel.RadiusX || int(math.Abs(float64(tap.DY))) > kernel.RadiusY {
			return schemaError(tapPath, "tap offset (%d,%d) exceeds declared support radius %dx%d",
				tap.DX, tap.DY, kernel.RadiusX, kernel.RadiusY)
		}
	}
	return nil
}

func requireKeys(obj map[string]any, path string, keys ...string) error {
	for _, key := range keys {
		value, ok := obj[key]
		if !ok {
			return schemaError(path+"."+key, "missing required field")
		}
		if value == nil {
			return schemaError(path+"."+key, "field must not be null")
		}
	}
	return nil
}

func asObject(value any, path string) (map[string]any, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, schemaError(path, "must be an object")
	}
	return obj, nil
}

func pathKey(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' || path[i] == ']' {
			return path[i+1:]
		}
	}
	return path
}

func requirePathObject(parent map[string]any, path string, keys ...string) (map[string]any, error) {
	key := pathKey(path)
	value, ok := parent[key]
	if !ok || value == nil {
		return nil, schemaError(path, "missing required field")
	}
	obj, err := asObject(value, path)
	if err != nil {
		return nil, err
	}
	if err := requireKeys(obj, path, keys...); err != nil {
		return nil, err
	}
	return obj, nil
}

func requirePathArray(parent map[string]any, path string, nonEmpty bool) ([]any, error) {
	key := pathKey(path)
	value, ok := parent[key]
	if !ok || value == nil {
		return nil, schemaError(path, "missing required field")
	}
	items, ok := value.([]any)
	if !ok {
		return nil, schemaError(path, "must be an array")
	}
	if nonEmpty && len(items) == 0 {
		return nil, schemaError(path, "array must not be empty")
	}
	return items, nil
}
