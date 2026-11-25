package independent

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"channelpipe"

	_ "modernc.org/sqlite"
)

type Violation struct {
	ImageID    string `json:"image_id,omitempty"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	StageIndex int    `json:"stage_index"`
	Field      string `json:"field"`
	Expected   string `json:"expected,omitempty"`
	Actual     string `json:"actual,omitempty"`
	Message    string `json:"message"`
}

type Report struct {
	OK                    bool              `json:"ok"`
	SampleDirectory       string            `json:"sample_directory"`
	DatabasePath          string            `json:"database_path"`
	TruthFingerprintOK    bool              `json:"truth_fingerprint_ok"`
	SnapshotIdentityOK    bool              `json:"snapshot_identity_ok"`
	CheckedSamples        int               `json:"checked_samples"`
	CheckedStagePoints    int               `json:"checked_stage_points"`
	CheckedPixels         int               `json:"checked_pixels"`
	CheckedDroppedRecords int               `json:"checked_dropped_records"`
	ImageCount            int               `json:"image_count"`
	FinalImageHashes      map[string]string `json:"final_image_hashes,omitempty"`
	Violation             *Violation        `json:"violation,omitempty"`
}

type manifestFile struct {
	PlanFingerprint  string            `json:"plan_fingerprint"`
	TruthFingerprint string            `json:"truth_fingerprint"`
	ParamVersion     string            `json:"param_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	PlanFiles        map[string]string `json:"plan_files"`
}

type truthFile struct {
	Fingerprint string        `json:"fingerprint"`
	Samples     []truthSample `json:"samples"`
}

type truthSample struct {
	ImageID string       `json:"image_id"`
	X       int          `json:"x"`
	Y       int          `json:"y"`
	Stages  []truthStage `json:"stages"`
}

type truthStage struct {
	StageIndex int      `json:"stage_index"`
	Channels   []string `json:"channels"`
	Values     []uint16 `json:"values"`
	Dropped    []string `json:"dropped"`
	Tolerance  uint16   `json:"tolerance"`
}

type rawImage struct {
	ID     string
	Width  int
	Height int
	Stages map[int]*rawBuffer
}

type rawBuffer struct {
	Width   int
	Height  int
	Depth   int
	Layout  string
	Pixels  []uint16
	Dropped []string
}

func Verify(sampleDir, databasePath string) (*Report, error) {
	report := &Report{OK: false, SampleDirectory: sampleDir, DatabasePath: databasePath}
	manifestBytes, err := os.ReadFile(filepath.Join(sampleDir, "manifest.json"))
	if err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeIO, err, "manifest.json", "independent verifier cannot read manifest")
	}
	var manifest manifestFile
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeSchema, err, "manifest.json", "invalid manifest")
	}
	truthBytes, err := os.ReadFile(filepath.Join(sampleDir, "truth", "truth.json"))
	if err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeIO, err, "truth.json", "cannot read truth")
	}
	var truth truthFile
	if err := json.Unmarshal(truthBytes, &truth); err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeSchema, err, "truth.json", "invalid truth")
	}
	truthOK, err := verifyTruthFingerprint(truthBytes, manifest.TruthFingerprint)
	if err != nil {
		return nil, err
	}
	report.TruthFingerprintOK = truthOK
	if !truthOK {
		report.Violation = &Violation{StageIndex: -1, Field: "truth_fingerprint", Expected: manifest.TruthFingerprint, Actual: truth.Fingerprint, Message: "truth fingerprint mismatch"}
		return report, nil
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, databasePath, "cannot open sqlite independently")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	identityOK, violation, err := verifyIdentity(db, manifest)
	if err != nil {
		return nil, err
	}
	report.SnapshotIdentityOK = identityOK
	if violation != nil {
		report.Violation = violation
		return report, nil
	}

	images, err := loadPersistedImages(db)
	if err != nil {
		return nil, err
	}
	report.ImageCount = len(images)
	report.FinalImageHashes = map[string]string{}
	imageIDs := make([]string, 0, len(images))
	for id := range images {
		imageIDs = append(imageIDs, id)
	}
	sort.Strings(imageIDs)

	for _, imageID := range imageIDs {
		image := images[imageID]
		maxStage := -1
		for stageIndex := range image.Stages {
			if stageIndex > maxStage {
				maxStage = stageIndex
			}
		}
		if maxStage >= 0 {
			raw := image.Stages[maxStage]
			channels := make([]channelpipe.Channel, 0, len(raw.Layout))
			for _, r := range raw.Layout {
				channels = append(channels, channelpipe.Channel(r))
			}
			hash, hashErr := channelpipe.RegionHash(&channelpipe.Buffer{Width: image.Width, Height: image.Height, Depth: raw.Depth, Channels: channels, Pixels: raw.Pixels})
			if hashErr != nil {
				return nil, hashErr
			}
			report.FinalImageHashes[imageID] = hash
		}
		for stageIndex := 0; ; stageIndex++ {
			buffer, ok := image.Stages[stageIndex]
			if !ok {
				break
			}
			report.CheckedPixels += buffer.Width * buffer.Height
			if violation := checkRange(imageID, stageIndex, buffer); violation != nil {
				report.Violation = violation
				return report, nil
			}
		}
	}

	report.CheckedSamples = len(truth.Samples)
	for _, sample := range truth.Samples {
		image := images[sample.ImageID]
		if image == nil {
			report.Violation = &Violation{ImageID: sample.ImageID, X: sample.X, Y: sample.Y, Field: "image_id", Message: "persisted image missing"}
			return report, nil
		}
		for _, expectedStage := range sample.Stages {
			buffer := image.Stages[expectedStage.StageIndex]
			if buffer == nil {
				report.Violation = &Violation{ImageID: sample.ImageID, X: sample.X, Y: sample.Y, StageIndex: expectedStage.StageIndex, Field: "stage_output", Message: "persisted stage output missing"}
				return report, nil
			}
			report.CheckedStagePoints++
			if sample.X < 0 || sample.X >= buffer.Width || sample.Y < 0 || sample.Y >= buffer.Height {
				report.Violation = &Violation{ImageID: sample.ImageID, X: sample.X, Y: sample.Y, StageIndex: expectedStage.StageIndex, Field: "coordinate", Message: "sample outside persisted buffer"}
				return report, nil
			}
			if violation := compareStageSample(sample, expectedStage, buffer); violation != nil {
				report.Violation = violation
				return report, nil
			}
			report.CheckedDroppedRecords++
		}
	}

	report.OK = true
	return report, nil
}

func verifyTruthFingerprint(truthBytes []byte, expected string) (bool, error) {
	var raw map[string]any
	if err := json.Unmarshal(truthBytes, &raw); err != nil {
		return false, channelpipe.WrapError(channelpipe.CodeSchema, err, "truth.json", "cannot parse truth for fingerprint")
	}
	delete(raw, "fingerprint")
	actual, err := channelpipe.Fingerprint(raw)
	if err != nil {
		return false, err
	}
	return actual == expected, nil
}

func verifyIdentity(db *sql.DB, manifest manifestFile) (bool, *Violation, error) {
	expected := map[string]string{
		"plan_fingerprint":  manifest.PlanFingerprint,
		"param_version":     manifest.ParamVersion,
		"algorithm_version": manifest.AlgorithmVersion,
	}
	for key, wanted := range expected {
		var actual string
		err := db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&actual)
		if err != nil {
			return false, nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "meta."+key, "cannot independently read snapshot identity")
		}
		if actual != wanted {
			return false, &Violation{StageIndex: -1, Field: "meta." + key, Expected: wanted, Actual: actual, Message: "snapshot identity mismatch"}, nil
		}
	}
	var badRows int
	err := db.QueryRow(`SELECT COUNT(*) FROM chunk_outputs WHERE plan_fingerprint<>? OR param_version<>? OR algorithm_version<>?`,
		manifest.PlanFingerprint, manifest.ParamVersion, manifest.AlgorithmVersion).Scan(&badRows)
	if err != nil {
		return false, nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunk_outputs", "cannot independently check row identities")
	}
	if badRows > 0 {
		return false, &Violation{StageIndex: -1, Field: "chunk_outputs.identity", Actual: fmt.Sprintf("%d mismatched rows", badRows), Message: "persisted row hash/version mismatch"}, nil
	}
	return true, nil, nil
}

func loadPersistedImages(db *sql.DB) (map[string]*rawImage, error) {
	chunkRows, err := db.Query(`SELECT image_id,chunk_index,x,y,width,height FROM chunks ORDER BY image_id,chunk_index`)
	if err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunks", "cannot read chunks independently")
	}
	type chunkRect struct {
		imageID    string
		index      int
		x, y, w, h int
	}
	var rects []chunkRect
	imageMap := map[string]*rawImage{}
	for chunkRows.Next() {
		var rect chunkRect
		if err := chunkRows.Scan(&rect.imageID, &rect.index, &rect.x, &rect.y, &rect.w, &rect.h); err != nil {
			_ = chunkRows.Close()
			return nil, err
		}
		rects = append(rects, rect)
		if imageMap[rect.imageID] == nil {
			imageMap[rect.imageID] = &rawImage{ID: rect.imageID, Stages: map[int]*rawBuffer{}}
		}
	}
	_ = chunkRows.Close()
	for _, rect := range rects {
		image := imageMap[rect.imageID]
		if rect.x+rect.w > image.Width {
			image.Width = rect.x + rect.w
		}
		if rect.y+rect.h > image.Height {
			image.Height = rect.y + rect.h
		}
	}

	outputRows, err := db.Query(`SELECT image_id,chunk_index,stage_index,width,height,channels,channel_layout,depth,blob,dropped_json FROM chunk_outputs ORDER BY image_id,chunk_index,stage_index`)
	if err != nil {
		return nil, channelpipe.WrapError(channelpipe.CodeCheckpoint, err, "chunk_outputs", "cannot read output blobs independently")
	}
	defer outputRows.Close()
	type tile struct {
		imageID               string
		chunkIndex            int
		stageIndex            int
		w, h, channels, depth int
		layout                string
		blob                  []byte
		droppedJSON           string
	}
	var tiles []tile
	for outputRows.Next() {
		var item tile
		if err := outputRows.Scan(&item.imageID, &item.chunkIndex, &item.stageIndex, &item.w, &item.h, &item.channels, &item.layout, &item.depth, &item.blob, &item.droppedJSON); err != nil {
			return nil, err
		}
		tiles = append(tiles, item)
	}
	rectByKey := map[string]chunkRect{}
	for _, rect := range rects {
		rectByKey[fmt.Sprintf("%s#%d", rect.imageID, rect.index)] = rect
	}
	for _, item := range tiles {
		image := imageMap[item.imageID]
		if image == nil {
			continue
		}
		buffer := image.Stages[item.stageIndex]
		if buffer == nil {
			buffer = &rawBuffer{Width: image.Width, Height: image.Height, Depth: item.depth, Layout: item.layout, Pixels: make([]uint16, image.Width*image.Height*item.channels)}
			image.Stages[item.stageIndex] = buffer
		}
		tilePixels, err := decodeIndependent(item.blob, item.w, item.h, item.channels, item.depth)
		if err != nil {
			return nil, err
		}
		rect := rectByKey[fmt.Sprintf("%s#%d", item.imageID, item.chunkIndex)]
		var dropped []string
		_ = json.Unmarshal([]byte(item.droppedJSON), &dropped)
		buffer.Dropped = append(buffer.Dropped, dropped...)
		copyTile(buffer.Pixels, tilePixels, image.Width, item.w, item.h, item.channels, rect.x, rect.y)
	}
	return imageMap, nil
}

func decodeIndependent(blob []byte, width, height, channels, depth int) ([]uint16, error) {
	count := width * height * channels
	pixels := make([]uint16, count)
	switch depth {
	case 8:
		if len(blob) != count {
			return nil, channelpipe.FieldError(channelpipe.CodeDiff, "blob_length", "8-bit blob length %d want %d", len(blob), count)
		}
		for i, value := range blob {
			pixels[i] = uint16(value)
		}
	case 16:
		if len(blob) != count*2 {
			return nil, channelpipe.FieldError(channelpipe.CodeDiff, "blob_length", "16-bit blob length %d want %d", len(blob), count*2)
		}
		for i := range pixels {
			pixels[i] = binary.LittleEndian.Uint16(blob[i*2 : i*2+2])
		}
	default:
		return nil, channelpipe.FieldError(channelpipe.CodeDiff, "depth", "unsupported persisted depth %d", depth)
	}
	return pixels, nil
}

func copyTile(destination, tile []uint16, fullWidth, tileWidth, tileHeight, channels, originX, originY int) {
	for y := 0; y < tileHeight; y++ {
		for x := 0; x < tileWidth; x++ {
			dstBase := ((originY+y)*fullWidth + originX + x) * channels
			srcBase := (y*tileWidth + x) * channels
			copy(destination[dstBase:dstBase+channels], tile[srcBase:srcBase+channels])
		}
	}
}

func checkRange(imageID string, stageIndex int, buffer *rawBuffer) *Violation {
	maxValue := uint16((1 << buffer.Depth) - 1)
	channels := len(buffer.Layout)
	if channels == 0 {
		channels = len(buffer.Pixels) / (buffer.Width * buffer.Height)
	}
	for y := 0; y < buffer.Height; y++ {
		for x := 0; x < buffer.Width; x++ {
			base := (y*buffer.Width + x) * channels
			for channel := 0; channel < channels; channel++ {
				value := buffer.Pixels[base+channel]
				if value > maxValue {
					field := fmt.Sprintf("channel[%d]", channel)
					if channel < len(buffer.Layout) {
						field = string(buffer.Layout[channel])
					}
					return &Violation{ImageID: imageID, X: x, Y: y, StageIndex: stageIndex, Field: field, Expected: fmt.Sprintf("<=%d", maxValue), Actual: fmt.Sprintf("%d", value), Message: "pixel value outside target depth range"}
				}
			}
		}
	}
	return nil
}

func compareStageSample(sample truthSample, expectedStage truthStage, buffer *rawBuffer) *Violation {
	channels := len(expectedStage.Channels)
	if channels == 0 {
		channels = len(expectedStage.Values)
	}
	base := (sample.Y*buffer.Width + sample.X) * channels
	tolerance := int(expectedStage.Tolerance)
	for channel := 0; channel < channels && channel < len(expectedStage.Values); channel++ {
		actual := int(buffer.Pixels[base+channel])
		expected := int(expectedStage.Values[channel])
		delta := actual - expected
		if delta < 0 {
			delta = -delta
		}
		if delta > tolerance {
			field := fmt.Sprintf("channel[%d]", channel)
			if channel < len(expectedStage.Channels) {
				field = expectedStage.Channels[channel]
			}
			return &Violation{
				ImageID: sample.ImageID, X: sample.X, Y: sample.Y, StageIndex: expectedStage.StageIndex, Field: field,
				Expected: fmt.Sprintf("%d±%d", expected, tolerance), Actual: fmt.Sprintf("%d", actual),
				Message: "sample value exceeds truth tolerance",
			}
		}
	}
	sort.Strings(expectedStage.Dropped)
	sort.Strings(buffer.Dropped)
	if !stringSlicesEqual(expectedStage.Dropped, uniqueStrings(buffer.Dropped)) {
		return &Violation{ImageID: sample.ImageID, X: sample.X, Y: sample.Y, StageIndex: expectedStage.StageIndex, Field: "dropped_channels", Expected: fmt.Sprintf("%v", expectedStage.Dropped), Actual: fmt.Sprintf("%v", uniqueStrings(buffer.Dropped)), Message: "dropped channel identifiers differ"}
	}
	return nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
