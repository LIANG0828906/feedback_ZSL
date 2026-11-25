package iconmask

import (
	"bytes"
	"encoding/json"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
)

func SaveProject(path string, project *Project) error {
	if project == nil {
		return schemaError("<project>", "project", "project is nil")
	}
	if err := ValidateProject(project); err != nil {
		return err
	}
	data, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return schemaError("<project>", "json", err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodeProject(data)
}

func DecodeProject(data []byte) (*Project, error) {
	var project Project
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&project); err != nil {
		return nil, schemaError("<json>", "project", "cannot decode project: "+err.Error())
	}
	if err := ValidateProject(&project); err != nil {
		return nil, err
	}
	return &project, nil
}

func ValidateProject(project *Project) *Error {
	if project == nil {
		return schemaError("<project>", "project", "project is nil")
	}
	if project.Format == "" {
		return schemaError("<project>", "format", "format is required")
	}
	if len(project.Images) == 0 {
		return schemaError("<project>", "images", "at least one image is required")
	}
	seenImages := make(map[string]bool, len(project.Images))
	for i := range project.Images {
		image := &project.Images[i]
		if err := validateImage(image); err != nil {
			return err
		}
		if seenImages[image.ID] {
			return schemaError(image.ID, "id", "duplicate image id")
		}
		seenImages[image.ID] = true
	}
	return nil
}

func validateImage(image *ImageSample) *Error {
	imageID := image.ID
	if imageID == "" {
		imageID = "<missing>"
	}
	if image.ID == "" {
		return schemaError(imageID, "id", "image id is required")
	}
	if image.Width <= 0 {
		return schemaError(imageID, "width", "width must be a positive integer")
	}
	if image.Height <= 0 {
		return schemaError(imageID, "height", "height must be a positive integer")
	}
	if image.InitialMask.Width != image.Width {
		return schemaError(imageID, "initialMask.width", "initial mask width does not match image width")
	}
	if image.InitialMask.Height != image.Height {
		return schemaError(imageID, "initialMask.height", "initial mask height does not match image height")
	}
	if len(image.InitialMask.Data) != image.Width*image.Height {
		return schemaError(imageID, "initialMask.data", "initial mask data length does not match width*height")
	}
	if !image.InitialMask.Binary() {
		return schemaError(imageID, "initialMask.data", "initial mask must be single-channel binary (0 or 1)")
	}
	if image.Truth.ReferenceMask.Width <= 0 {
		return schemaError(imageID, "truth.referenceMask.width", "reference mask width is required")
	}
	if image.Truth.ReferenceMask.Height <= 0 {
		return schemaError(imageID, "truth.referenceMask.height", "reference mask height is required")
	}
	if image.Truth.ReferenceMask.Width != image.Width {
		return schemaError(imageID, "truth.referenceMask.width", "reference mask width does not match image width")
	}
	if image.Truth.ReferenceMask.Height != image.Height {
		return schemaError(imageID, "truth.referenceMask.height", "reference mask height does not match image height")
	}
	if len(image.Truth.ReferenceMask.Data) != image.Width*image.Height {
		return schemaError(imageID, "truth.referenceMask.data", "reference mask data length does not match width*height")
	}
	if !image.Truth.ReferenceMask.Binary() {
		return schemaError(imageID, "truth.referenceMask.data", "reference mask must be single-channel binary (0 or 1)")
	}
	if len(image.ImagePNG) == 0 {
		return schemaError(imageID, "imagePng", "image PNG is required")
	}
	if len(image.ImagePNG) > 0 {
		decoded, err := png.Decode(bytes.NewReader(image.ImagePNG))
		if err != nil {
			return schemaError(imageID, "imagePng", "cannot decode image: "+err.Error())
		}
		bounds := decoded.Bounds()
		if bounds.Dx() != image.Width || bounds.Dy() != image.Height {
			return schemaError(imageID, "imagePng", "image dimensions do not match image width/height")
		}
	}
	if len(image.Truth.Components) == 0 {
		return schemaError(imageID, "truth.components", "at least one component is required")
	}
	componentIDs := make(map[string]bool, len(image.Truth.Components))
	for i, component := range image.Truth.Components {
		field := "truth.components[" + itoa(i) + "]"
		if component.ID == "" {
			return schemaError(imageID, field+".id", "component id is required")
		}
		if componentIDs[component.ID] {
			return schemaError(imageID, field+".id", "duplicate component id")
		}
		componentIDs[component.ID] = true
		if component.Area <= 0 {
			return schemaError(imageID, field+".area", "component area must be positive")
		}
		if component.BBox.Empty() {
			return schemaError(imageID, field+".bbox", "component bbox must be non-empty")
		}
		if component.BBox.X < 0 || component.BBox.Y < 0 ||
			component.BBox.X+component.BBox.Width > image.Width ||
			component.BBox.Y+component.BBox.Height > image.Height {
			return schemaError(imageID, field+".bbox", "component bbox is outside the image")
		}
		if !finite(component.Centroid.X) || !finite(component.Centroid.Y) {
			return schemaError(imageID, field+".centroid", "component centroid must be finite")
		}
		if component.Holes < 0 {
			return schemaError(imageID, field+".holes", "component hole count must be non-negative")
		}
	}
	if len(image.Truth.Segments) == 0 {
		return schemaError(imageID, "truth.segments", "at least one boundary segment is required")
	}
	if err := validateSegments(imageID, image); err != nil {
		return err
	}
	return nil
}

func validateSegments(imageID string, image *ImageSample) *Error {
	loopSegments := make(map[string][]BoundarySegment)
	segmentIDs := make(map[string]bool, len(image.Truth.Segments))
	for i, segment := range image.Truth.Segments {
		field := "truth.segments[" + itoa(i) + "]"
		if segment.ID == "" {
			return schemaError(imageID, field+".id", "segment id is required")
		}
		if segmentIDs[segment.ID] {
			return schemaError(imageID, field+".id", "duplicate segment id")
		}
		segmentIDs[segment.ID] = true
		if segment.ComponentID == "" {
			return schemaError(imageID, field+".componentId", "segment component id is required")
		}
		if _, ok := componentByID(image.Truth.Components, segment.ComponentID); !ok {
			return schemaError(imageID, field+".componentId", "segment references an unknown component")
		}
		if segment.Loop == "" {
			return schemaError(imageID, field+".loop", "segment loop is required")
		}
		if !finite(segment.Start) || !finite(segment.End) {
			return schemaError(imageID, field+".start", "segment boundaries must be finite")
		}
		if segment.Start < 0 || segment.End > 1 || segment.Start >= segment.End {
			return schemaError(imageID, field+".start", "segment must satisfy 0 <= start < end <= 1")
		}
		if !finite(segment.Curvature) || segment.Curvature < 0 {
			return schemaError(imageID, field+".curvature", "segment curvature must be finite and non-negative")
		}
		for j, point := range segment.Points {
			if !finite(point.X) || !finite(point.Y) ||
				point.X < 0 || point.Y < 0 || point.X > float64(image.Width) || point.Y > float64(image.Height) {
				return schemaError(imageID, field+".points["+itoa(j)+"]", "segment point is outside the image")
			}
		}
		key := segment.ComponentID + "\x00" + segment.Loop
		loopSegments[key] = append(loopSegments[key], segment)
	}
	if err := validateComponentLoops(imageID, image, loopSegments); err != nil {
		return err
	}
	for key, segments := range loopSegments {
		parts := splitLoopKey(key)
		if err := validateLoopCoverage(imageID, parts[0], parts[1], segments); err != nil {
			return err
		}
	}
	return nil
}

func validateComponentLoops(imageID string, image *ImageSample, loops map[string][]BoundarySegment) *Error {
	for _, component := range image.Truth.Components {
		foundOuter := false
		for key := range loops {
			if len(key) > len(component.ID) && key[:len(component.ID)+1] == component.ID+"\x00" {
				parts := splitLoopKey(key)
				if parts[1] == "outer" {
					foundOuter = true
					break
				}
			}
		}
		if !foundOuter {
			return schemaError(imageID, "truth.segments", "component "+component.ID+" has no outer boundary segments")
		}
	}
	return nil
}

func validateLoopCoverage(imageID, componentID, loop string, segments []BoundarySegment) *Error {
	if len(segments) == 0 {
		return schemaError(imageID, "truth.segments", "boundary loop has no segments")
	}
	if !approxEqual(segments[0].Start, 0) {
		return schemaError(imageID, "truth.segments.start", "boundary loop must start at 0 for "+componentID+"/"+loop)
	}
	previousEnd := 0.0
	previousStart := -1.0
	for i, segment := range segments {
		if i > 0 {
			if !approxEqual(segment.Start, previousEnd) {
				return schemaError(imageID, "truth.segments.start", "boundary segments must be contiguous for "+componentID+"/"+loop)
			}
			if segment.Start <= previousStart {
				return schemaError(imageID, "truth.segments.start", "segment starts must be strictly increasing")
			}
		}
		previousStart = segment.Start
		previousEnd = segment.End
	}
	if !approxEqual(previousEnd, 1) {
		return schemaError(imageID, "truth.segments.end", "boundary loop must end at 1 for "+componentID+"/"+loop)
	}
	return nil
}

func componentByID(components []ComponentTruth, id string) (ComponentTruth, bool) {
	for _, component := range components {
		if component.ID == id {
			return component, true
		}
	}
	return ComponentTruth{}, false
}

func splitLoopKey(key string) [2]string {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return [2]string{key[:i], key[i+1:]}
		}
	}
	return [2]string{key, ""}
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// NormalizeProject returns a stable ordering of segments and components. It is
// used by diagnostics and tests; the loader deliberately rejects malformed
// ordering rather than silently repairing it.
func NormalizeProject(project *Project) {
	if project == nil {
		return
	}
	for i := range project.Images {
		image := &project.Images[i]
		sort.SliceStable(image.Truth.Components, func(a, b int) bool {
			return image.Truth.Components[a].ID < image.Truth.Components[b].ID
		})
		sort.SliceStable(image.Truth.Segments, func(a, b int) bool {
			sa, sb := image.Truth.Segments[a], image.Truth.Segments[b]
			if sa.ComponentID != sb.ComponentID {
				return sa.ComponentID < sb.ComponentID
			}
			if sa.Loop != sb.Loop {
				return sa.Loop < sb.Loop
			}
			return sa.Start < sb.Start
		})
	}
}
