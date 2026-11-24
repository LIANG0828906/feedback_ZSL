package compose

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	"virtualstudio/model"
)

type RenderOptions struct {
	Samples []model.SamplePoint
}

func DefaultSamples(width, height int) []model.SamplePoint {
	return []model.SamplePoint{
		{X: width / 16, Y: height / 8},
		{X: width / 3, Y: height / 2},
		{X: width / 2, Y: height / 2},
		{X: (width * 2) / 3, Y: height / 3},
		{X: (width * 3) / 4, Y: (height * 2) / 3},
		{X: width - width/16 - 1, Y: height - height/8 - 1},
	}
}

func validateRender(scene *model.Scene, commands *model.CommandSet, frame int, opts RenderOptions) error {
	if err := model.ValidateScene(scene); err != nil {
		return err
	}
	if frame < 0 || frame >= scene.FrameCount {
		return model.NewError(model.CodeParam, "frame", fmt.Sprintf("must be in [0,%d)", scene.FrameCount))
	}
	for i, point := range opts.Samples {
		if point.X < 0 || point.X >= scene.Width || point.Y < 0 || point.Y >= scene.Height {
			return model.NewError(model.CodeParam, fmt.Sprintf("sample_points[%d]", i), "sample is outside the frame")
		}
	}
	if commands != nil {
		if len(commands.Frames) != scene.FrameCount {
			return model.NewError(model.CodeVersion, "commands.frames", "count does not match scene frame_count")
		}
	}
	return nil
}

func RenderFrame(scene *model.Scene, commands *model.CommandSet, frame int, opts RenderOptions) (model.Frame, error) {
	if opts.Samples == nil {
		opts.Samples = DefaultSamples(scene.Width, scene.Height)
	}
	if err := validateRender(scene, commands, frame, opts); err != nil {
		return model.Frame{}, err
	}
	pixels := scene.Width * scene.Height
	premul := make([]float64, pixels*4)
	visible := make([]string, 0, len(scene.Layers))
	for _, layer := range scene.Layers {
		cmd := commandAt(commands, frame)
		eff := model.EffectiveLayerState(scene, cmd, layer)
		if !eff.Visible || eff.Opacity <= 0 {
			continue
		}
		if len(layer.Pixels) != pixels*scene.Channels {
			return model.Frame{}, model.NewError(model.CodeDecode, "layers."+layer.ID+".pixels", "pixel buffer length mismatch")
		}
		visible = append(visible, layer.ID)
		for p := 0; p < pixels; p++ {
			off := p * 4
			a := (float64(layer.Pixels[off+3]) / 255.0) * eff.Opacity
			if a <= 0 {
				continue
			}
			if a > 1 {
				a = 1
			}
			r := DecodeSRGB(layer.Pixels[off]) * a
			g := DecodeSRGB(layer.Pixels[off+1]) * a
			b := DecodeSRGB(layer.Pixels[off+2]) * a
			inv := 1.0 - a
			premul[off] = r + premul[off]*inv
			premul[off+1] = g + premul[off+1]*inv
			premul[off+2] = b + premul[off+2]*inv
			premul[off+3] = a + premul[off+3]*inv
		}
	}

	rgba := make([]byte, pixels*4)
	var sum [4]float64
	var minLuma, maxLuma uint8
	for p := 0; p < pixels; p++ {
		off := p * 4
		a := clamp01(premul[off+3])
		if a <= 0 {
			rgba[off], rgba[off+1], rgba[off+2], rgba[off+3] = 0, 0, 0, 0
		} else {
			rgba[off] = EncodeSRGB(clamp01(premul[off] / a))
			rgba[off+1] = EncodeSRGB(clamp01(premul[off+1] / a))
			rgba[off+2] = EncodeSRGB(clamp01(premul[off+2] / a))
			rgba[off+3] = uint8(math.Round(a * 255.0))
		}
		luma := uint8(math.Round(0.2126*float64(rgba[off]) + 0.7152*float64(rgba[off+1]) + 0.0722*float64(rgba[off+2])))
		if p == 0 || luma < minLuma {
			minLuma = luma
		}
		if p == 0 || luma > maxLuma {
			maxLuma = luma
		}
		sum[0] += float64(rgba[off])
		sum[1] += float64(rgba[off+1])
		sum[2] += float64(rgba[off+2])
		sum[3] += float64(rgba[off+3])
	}
	stats := model.FrameStats{
		Frame:         frame,
		TimeNS:        scene.TimeForFrame(frame),
		VisibleLayers: visible,
		MinLuma:       minLuma,
		MaxLuma:       maxLuma,
		MeanRGBA: [4]float64{
			sum[0] / float64(pixels),
			sum[1] / float64(pixels),
			sum[2] / float64(pixels),
			sum[3] / float64(pixels),
		},
	}
	for _, point := range opts.Samples {
		off := (point.Y*scene.Width + point.X) * scene.Channels
		var sample [4]uint8
		copy(sample[:], rgba[off:off+4])
		stats.Samples = append(stats.Samples, model.PixelSample{X: point.X, Y: point.Y, RGBA: sample})
	}
	frameOut := model.Frame{
		Index:    frame,
		Width:    scene.Width,
		Height:   scene.Height,
		Channels: scene.Channels,
		TimeNS:   scene.TimeForFrame(frame),
		RGBA:     rgba,
		Stats:    stats,
	}
	frameOut.Hash = HashFrame(frameOut)
	stats.ContentHash = frameOut.Hash
	frameOut.Stats = stats
	return frameOut, nil
}

func RenderWindow(scene *model.Scene, commands *model.CommandSet, startFrame, endFrame int, opts RenderOptions) ([]model.Frame, error) {
	if scene == nil {
		return nil, model.NewError(model.CodeVersion, "scene", "scene is nil")
	}
	if startFrame < 0 || endFrame < startFrame || endFrame >= scene.FrameCount {
		return nil, model.NewError(model.CodeParam, "window", fmt.Sprintf("invalid frame range [%d,%d]", startFrame, endFrame))
	}
	if opts.Samples == nil {
		opts.Samples = DefaultSamples(scene.Width, scene.Height)
	}
	if err := validateRender(scene, commands, startFrame, opts); err != nil {
		return nil, err
	}
	frames := make([]model.Frame, 0, endFrame-startFrame+1)
	for frame := startFrame; frame <= endFrame; frame++ {
		out, err := RenderFrame(scene, commands, frame, opts)
		if err != nil {
			return nil, err
		}
		frames = append(frames, out)
	}
	return frames, nil
}

func RenderAll(scene *model.Scene, commands *model.CommandSet, opts RenderOptions) ([]model.Frame, error) {
	return RenderWindow(scene, commands, 0, scene.FrameCount-1, opts)
}

func commandAt(commands *model.CommandSet, frame int) *model.FrameCommand {
	if commands == nil {
		return nil
	}
	if frame >= 0 && frame < len(commands.Frames) && commands.Frames[frame].Frame == frame {
		return &commands.Frames[frame]
	}
	for i := range commands.Frames {
		if commands.Frames[i].Frame == frame {
			return &commands.Frames[i]
		}
	}
	return nil
}

func HashFrame(frame model.Frame) string {
	h := sha256.New()
	var header [32]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(frame.Index))
	binary.LittleEndian.PutUint32(header[4:8], uint32(frame.Width))
	binary.LittleEndian.PutUint32(header[8:12], uint32(frame.Height))
	binary.LittleEndian.PutUint32(header[12:16], uint32(frame.Channels))
	binary.LittleEndian.PutUint64(header[16:24], uint64(frame.TimeNS))
	binary.LittleEndian.PutUint64(header[24:32], uint64(len(frame.RGBA)))
	_, _ = h.Write(header[:])
	_, _ = h.Write(frame.RGBA)
	return hex.EncodeToString(h.Sum(nil))
}

func WindowDigest(frames []model.Frame) string {
	h := sha256.New()
	for _, frame := range frames {
		_, _ = h.Write([]byte(frame.Hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func VisibleLayerIDs(scene *model.Scene, commands *model.CommandSet, frame int) []string {
	out := make([]string, 0, len(scene.Layers))
	for _, layer := range scene.Layers {
		eff := model.EffectiveLayerState(scene, commandAt(commands, frame), layer)
		if eff.Visible && eff.Opacity > 0 {
			out = append(out, layer.ID)
		}
	}
	sort.Strings(out)
	return out
}
