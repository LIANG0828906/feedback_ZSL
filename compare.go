package channelpipe

import "fmt"

type BufferSummary struct {
	StageIndex int       `json:"stage_index"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	Depth      int       `json:"depth"`
	Channels   []Channel `json:"channels"`
	PixelCount int       `json:"pixel_count"`
	Hash       string    `json:"hash"`
}

type Mismatch struct {
	ImageID    string `json:"image_id,omitempty"`
	StageIndex int    `json:"stage_index"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	Field      string `json:"field"`
	Expected   uint16 `json:"expected"`
	Actual     uint16 `json:"actual"`
	Message    string `json:"message"`
}

func SummarizeBuffer(stageIndex int, buffer *Buffer) (BufferSummary, error) {
	hash, err := RegionHash(buffer)
	if err != nil {
		return BufferSummary{}, err
	}
	if buffer == nil {
		return BufferSummary{StageIndex: stageIndex, Hash: SHA256Hex([]byte("nil"))}, nil
	}
	return BufferSummary{
		StageIndex: stageIndex,
		Width:      buffer.Width,
		Height:     buffer.Height,
		Depth:      buffer.Depth,
		Channels:   append([]Channel(nil), buffer.Channels...),
		PixelCount: buffer.PixelCount(),
		Hash:       hash,
	}, nil
}

func CompareBuffers(imageID string, stageIndex int, expected, actual *Buffer) *Mismatch {
	if expected == nil || actual == nil {
		return &Mismatch{ImageID: imageID, StageIndex: stageIndex, Field: "buffer", Message: "one buffer is nil"}
	}
	if expected.Width != actual.Width || expected.Height != actual.Height {
		return &Mismatch{ImageID: imageID, StageIndex: stageIndex, Field: "dimensions", Message: fmt.Sprintf("expected %dx%d, actual %dx%d", expected.Width, expected.Height, actual.Width, actual.Height)}
	}
	if expected.Depth != actual.Depth {
		return &Mismatch{ImageID: imageID, StageIndex: stageIndex, X: 0, Y: 0, Field: "depth", Expected: uint16(expected.Depth), Actual: uint16(actual.Depth)}
	}
	if len(expected.Channels) != len(actual.Channels) {
		return &Mismatch{ImageID: imageID, StageIndex: stageIndex, Field: "channel_count", Expected: uint16(len(expected.Channels)), Actual: uint16(len(actual.Channels))}
	}
	for i := range expected.Channels {
		if expected.Channels[i] != actual.Channels[i] {
			return &Mismatch{ImageID: imageID, StageIndex: stageIndex, Field: "channel_layout[" + itoa(i) + "]", Message: fmt.Sprintf("expected %s, actual %s", expected.Channels[i], actual.Channels[i])}
		}
	}
	channels := expected.ChannelCount()
	for y := 0; y < expected.Height; y++ {
		for x := 0; x < expected.Width; x++ {
			base := (y*expected.Width + x) * channels
			for channel := 0; channel < channels; channel++ {
				if expected.Pixels[base+channel] != actual.Pixels[base+channel] {
					field := string(expected.Channels[channel])
					return &Mismatch{
						ImageID:    imageID,
						StageIndex: stageIndex,
						X:          x,
						Y:          y,
						Field:      field,
						Expected:   expected.Pixels[base+channel],
						Actual:     actual.Pixels[base+channel],
						Message:    "pixel channel differs",
					}
				}
			}
		}
	}
	return nil
}

func CompareStageSlices(imageID string, expected, actual []*Buffer) *Mismatch {
	if len(expected) != len(actual) {
		return &Mismatch{ImageID: imageID, Field: "stage_count", Expected: uint16(len(expected)), Actual: uint16(len(actual)), Message: "stage count differs"}
	}
	for stageIndex := range expected {
		if mismatch := CompareBuffers(imageID, stageIndex, expected[stageIndex], actual[stageIndex]); mismatch != nil {
			return mismatch
		}
	}
	return nil
}

func DiffError(mismatch *Mismatch) *Error {
	if mismatch == nil {
		return nil
	}
	return NewError(CodeDiff, "pixel mismatch at %s stage=%d x=%d y=%d field=%s expected=%d actual=%d", mismatch.ImageID, mismatch.StageIndex, mismatch.X, mismatch.Y, mismatch.Field, mismatch.Expected, mismatch.Actual).
		withDetails(map[string]any{"mismatch": mismatch})
}
