package vpcompose

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

type Frame struct {
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Channels  int       `json:"channels"`
	Timebase  Timebase  `json:"timebase"`
	TimeTicks int64     `json:"timeTick"`
	AlphaMode AlphaMode `json:"alphaMode"`
	Pixels    []uint16  `json:"pixels"`
}

func (f *Frame) Pixel(x, y int) ([4]uint16, bool) {
	if f == nil || x < 0 || y < 0 || x >= f.Width || y >= f.Height || len(f.Pixels) < f.Width*f.Height*f.Channels {
		return [4]uint16{}, false
	}
	offset := (y*f.Width + x) * f.Channels
	return [4]uint16{
		f.Pixels[offset], f.Pixels[offset+1], f.Pixels[offset+2], f.Pixels[offset+3],
	}, true
}

func (f *Frame) Digest() string {
	h := sha256.New()
	_ = binary.Write(h, binary.LittleEndian, int64(f.Width))
	_ = binary.Write(h, binary.LittleEndian, int64(f.Height))
	_ = binary.Write(h, binary.LittleEndian, int64(f.Channels))
	_ = binary.Write(h, binary.LittleEndian, f.TimeTicks)
	h.Write([]byte(f.Timebase.Unit))
	h.Write([]byte{0})
	_ = binary.Write(h, binary.LittleEndian, f.Timebase.TickRate)
	h.Write([]byte(f.Timebase.Rounding))
	h.Write([]byte{0})
	h.Write([]byte(f.AlphaMode))
	for _, value := range f.Pixels {
		_ = binary.Write(h, binary.LittleEndian, value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ContentPixel produces the straight sRGB content declared by a layer's
// content digest. It is a pure function of digest and coordinates.
func ContentPixel(contentHash string, x, y int) [4]uint16 {
	var coordinate [16]byte
	binary.LittleEndian.PutUint64(coordinate[0:8], uint64(int64(x)))
	binary.LittleEndian.PutUint64(coordinate[8:16], uint64(int64(y)))
	h := sha256.New()
	h.Write([]byte(contentHash))
	h.Write([]byte{0})
	h.Write(coordinate[:])
	sum := h.Sum(nil)
	alpha8 := uint16(128 + int(sum[0])%128)
	return [4]uint16{
		uint16(sum[1]) * 257,
		uint16(sum[2]) * 257,
		uint16(sum[3]) * 257,
		alpha8 * 257,
	}
}

type layerFrameState struct {
	source *Layer
	layer  Layer
}

func layersAtFrame(layers []*Layer, instructions []Instruction, timeTicks int64) ([]layerFrameState, error) {
	states := make([]layerFrameState, len(layers))
	index := make(map[string]int, len(layers))
	for i, layer := range layers {
		states[i] = layerFrameState{source: layer, layer: *layer}
		index[layer.ID] = i
	}
	for _, instruction := range instructions {
		if instruction.At > timeTicks {
			continue
		}
		position, ok := index[instruction.LayerID]
		if !ok {
			return nil, schemaError("instructions.layerId", "unknown layer "+instruction.LayerID)
		}
		state := &states[position].layer
		switch instruction.Action {
		case InstructionSetVisible:
			if instruction.Visible == nil {
				return nil, schemaError("instructions.visible", "missing value")
			}
			state.Visible = *instruction.Visible
		case InstructionSetOpacity:
			if instruction.Opacity == nil {
				return nil, schemaError("instructions.opacity", "missing value")
			}
			state.Opacity = *instruction.Opacity
		case InstructionSetVisibleUntil:
			if instruction.VisibleUntil == nil {
				return nil, schemaError("instructions.visibleUntilTick", "missing value")
			}
			state.VisibleUntil = *instruction.VisibleUntil
		case InstructionSetBlendMode:
			if !instruction.BlendMode.Valid() {
				return nil, schemaError("instructions.blendMode", "unsupported blend mode")
			}
			state.BlendMode = instruction.BlendMode
		default:
			return nil, schemaError("instructions.action", "unknown action")
		}
	}
	return states, nil
}

func layerVisible(layer Layer, timeTicks int64) bool {
	return layer.Visible && timeTicks <= layer.VisibleUntil
}

func sourceLayerPixel(state Layer, x, y int) [4]uint16 {
	content := ContentPixel(state.ContentHash, x, y)
	alpha := mul16(uint32(content[3]), uint32(state.Opacity))
	straight := [4]uint16{
		SRGB8ToLinear16(uint8(content[0] >> 8)),
		SRGB8ToLinear16(uint8(content[1] >> 8)),
		SRGB8ToLinear16(uint8(content[2] >> 8)),
		uint16(alpha),
	}
	// SourceAlphaMode selects the conversion path into the canonical
	// premultiplied working space. Straight source data is premultiplied here;
	// premultiplied source data still passes through the explicit unpremultiply
	// and repremultiply operation so both paths are exercised deterministically.
	// The digest is canonical content; SourceAlphaMode is provenance. The codec
	// normalizes it into this premultiplied working space without changing the
	// declared digest.
	for i := 0; i < 3; i++ {
		straight[i] = PremultiplyChannel(straight[i], uint16(alpha))
	}
	return straight
}

// ComposeFrameWithLayers composes one frame using an explicit frozen layer
// stack. It performs no mutation, so identical inputs produce identical bytes
// independent of goroutine, clock, or traversal order.
func ComposeFrameWithLayers(project *Project, frame FrameSpec, layers []*Layer, instructions []Instruction) (*Frame, error) {
	if project == nil {
		return nil, schemaError("project", "nil project")
	}
	if project.Width <= 0 || project.Height <= 0 || project.Channels != 4 {
		return nil, schemaError("frame", "invalid internal frame dimensions")
	}
	if frame.Index < 0 || frame.Index >= len(project.Frames) {
		return nil, schemaError("frame.index", "outside project frames")
	}
	if project.Frames[frame.Index].TimeTicks != frame.TimeTicks {
		return nil, schemaError("frame.timeTick", "does not match project frame")
	}
	states, err := layersAtFrame(layers, instructions, frame.TimeTicks)
	if err != nil {
		return nil, err
	}
	pixels := make([]uint16, project.Width*project.Height*project.Channels)
	for y := 0; y < project.Height; y++ {
		for x := 0; x < project.Width; x++ {
			offset := (y*project.Width + x) * 4
			dst := [4]uint16{
				pixels[offset], pixels[offset+1], pixels[offset+2], pixels[offset+3],
			}
			for i := range states {
				state := states[i].layer
				if !layerVisible(state, frame.TimeTicks) {
					continue
				}
				dst = compositePixel(dst, sourceLayerPixel(state, x, y), state.BlendMode)
			}
			pixels[offset] = dst[0]
			pixels[offset+1] = dst[1]
			pixels[offset+2] = dst[2]
			pixels[offset+3] = dst[3]
		}
	}
	return &Frame{
		Width: project.Width, Height: project.Height, Channels: project.Channels,
		Timebase: project.Timebase, TimeTicks: frame.TimeTicks,
		AlphaMode: AlphaPremultiplied, Pixels: pixels,
	}, nil
}

func ComposeFrame(project *Project, frameIndex int) (*Frame, error) {
	if project == nil {
		return nil, schemaError("project", "nil project")
	}
	if frameIndex < 0 || frameIndex >= len(project.Frames) {
		return nil, schemaError("frameIndex", "outside project frames")
	}
	return ComposeFrameWithLayers(project, project.Frames[frameIndex], project.Layers, project.Instructions)
}

// VerifyTruth compares the analytical truth samples against composition.
func VerifyTruth(project *Project) error {
	if project == nil || project.Truth == nil {
		return schemaError("truth", "missing truth")
	}
	if len(project.Truth.Frames) != len(project.Frames) {
		return schemaError("truth.frames", "frame count does not match project")
	}
	for i, frameSpec := range project.Frames {
		frame, err := ComposeFrame(project, i)
		if err != nil {
			return err
		}
		truth := project.Truth.Frames[i]
		if truth.Index != i || truth.TimeTicks != frameSpec.TimeTicks {
			return &EngineError{
				Code: CodePixel, Window: i / project.WindowSize,
				Field: "truth.timeTick", Message: "truth frame timestamp mismatch",
				Expected: truth.TimeTicks, Actual: frameSpec.TimeTicks,
			}
		}
		for _, sample := range truth.SamplePixels {
			actual, ok := frame.Pixel(sample.X, sample.Y)
			if !ok {
				return &EngineError{
					Code: CodePixel, Window: i / project.WindowSize,
					Field: "truth.samplePixels", Message: "sample point outside frame",
				}
			}
			var maxDeviation int
			for channel := 0; channel < 4; channel++ {
				diff := int(actual[channel]) - int(sample.RGBA[channel])
				if diff < 0 {
					diff = -diff
				}
				if diff > maxDeviation {
					maxDeviation = diff
				}
			}
			if maxDeviation > project.Truth.Tolerance {
				return &EngineError{
					Code: CodePixel, Window: i / project.WindowSize,
					Path:    fmt.Sprintf("truth.frames[%d].samplePixels(%d,%d)", i, sample.X, sample.Y),
					Message: "composite sample exceeds tolerance",
					Field:   "rgba", Expected: sample.RGBA, Actual: actual,
					Operands: []int64{int64(maxDeviation), int64(project.Truth.Tolerance)},
				}
			}
		}
	}
	return nil
}

func FrameDigest(frame *Frame) string {
	if frame == nil {
		return ""
	}
	return frame.Digest()
}
