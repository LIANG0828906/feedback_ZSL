package dataset

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"

	"screenverify/internal/frameio"
	"screenverify/internal/model"
	"screenverify/internal/rverr"
)

func Load(root string) (*model.FrameSet, error) {
	truthPath := filepath.Join(root, "truth.json")
	raw, err := os.ReadFile(truthPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, rverr.New(rverr.EDecode, "truth manifest missing: "+truthPath)
		}
		return nil, rverr.Wrap(rverr.EDecode, "read truth manifest", err)
	}
	truth := &model.Truth{}
	if err := json.Unmarshal(raw, truth); err != nil {
		return nil, rverr.Wrap(rverr.EDecode, "parse truth manifest", err)
	}
	if err := validateManifest(truth); err != nil {
		return nil, err
	}

	set := &model.FrameSet{Root: root, TruthPath: truthPath, Truth: truth}
	var lastTS int64
	for i := range truth.Frames {
		ft := truth.Frames[i]
		frameNo := i + 1
		if ft.Frame != frameNo {
			return nil, rverr.Field(rverr.EMeta, "frames["+itoa(i)+"].frame", "frame number does not match manifest order")
		}
		if i > 0 && ft.TimestampNS < lastTS {
			return nil, rverr.Frame(rverr.EOrder, ft.Frame, "timestamp_ns", "timestamp is smaller than previous frame")
		}
		lastTS = ft.TimestampNS
		if !ft.WatermarkRegion.ValidFor(truth.Width, truth.Height) {
			return nil, rverr.Frame(rverr.ERange, ft.Frame, "watermark_region", "watermark region is outside frame or has invalid size")
		}
		if err := validatePrimitives(ft.Primitives, ft.Frame); err != nil {
			return nil, err
		}

		framePath := filepath.Join(root, filepath.FromSlash(ft.File))
		frame, err := frameio.ReadFile(framePath)
		if err != nil {
			if e := rverr.As(err); e != nil && e.Frame == 0 {
				e.Frame = ft.Frame
			}
			return nil, err
		}
		if frame.Width != truth.Width {
			return nil, rverr.Frame(rverr.EMeta, ft.Frame, "width", "frame header width does not match manifest")
		}
		if frame.Height != truth.Height {
			return nil, rverr.Frame(rverr.EMeta, ft.Frame, "height", "frame header height does not match manifest")
		}
		if frame.Channels != truth.Channels {
			return nil, rverr.Frame(rverr.EMeta, ft.Frame, "channels", "frame header channels does not match manifest")
		}
		hash := model.HashBytes("actual-frame-v1:", frame.Pixels)
		if ft.ContentSHA256 != "" && hash != ft.ContentSHA256 {
			return nil, rverr.Frame(rverr.EDecode, ft.Frame, "content_sha256", "decoded pixels do not match content hash")
		}
		frame.No = ft.Frame
		frame.TimestampNS = ft.TimestampNS
		frame.ContentHash = hash
		set.Frames = append(set.Frames, frame)

		basePath := filepath.Join(root, filepath.FromSlash(ft.BaselineFile))
		base, err := frameio.ReadFile(basePath)
		if err != nil {
			if e := rverr.As(err); e != nil && e.Frame == 0 {
				e.Frame = ft.Frame
			}
			return nil, err
		}
		baseHash := model.HashBytes("baseline-frame-v1:", base.Pixels)
		if ft.BaselineSHA256 != "" && baseHash != ft.BaselineSHA256 {
			return nil, rverr.Frame(rverr.EDecode, ft.Frame, "baseline_sha256", "baseline pixels do not match hash")
		}
		set.Baselines = append(set.Baselines, base.Pixels)
	}
	return set, nil
}

func validateManifest(t *model.Truth) error {
	if t.Width <= 0 {
		return rverr.Field(rverr.EMeta, "width", "width must be positive")
	}
	if t.Height <= 0 {
		return rverr.Field(rverr.EMeta, "height", "height must be positive")
	}
	if t.Channels != 3 && t.Channels != 4 {
		return rverr.Field(rverr.EMeta, "channels", "channels must be 3 or 4")
	}
	if t.FrameCount <= 0 {
		return rverr.Field(rverr.EMeta, "frame_count", "frame_count must be positive")
	}
	if len(t.Frames) != t.FrameCount {
		return rverr.Field(rverr.EMeta, "frame_count", "frame_count does not match length of frames list")
	}
	r := t.Replacement
	if r.StartFrame != -1 || r.EndFrame != -1 {
		if r.StartFrame < 1 || r.EndFrame < r.StartFrame || r.EndFrame > t.FrameCount {
			return rverr.Field(rverr.EMeta, "replacement.interval", "replacement interval is outside frame list")
		}
		if len(r.Mask) != t.FrameCount {
			return rverr.Field(rverr.EMeta, "replacement.mask", "replacement mask length does not match frame_count")
		}
	}
	return nil
}

func validatePrimitives(ps []model.Primitive, frame int) error {
	for i, p := range ps {
		coords := []float64{
			float64(p.BBox.X), float64(p.BBox.Y), float64(p.BBox.Width), float64(p.BBox.Height),
			p.BBox.XFloat, p.BBox.YFloat, p.BBox.WFloat, p.BBox.HFloat,
		}
		for _, v := range coords {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return rverr.Frame(rverr.EMeta, frame, "primitives["+itoa(i)+"].bbox", "bounding box coordinate is not finite")
			}
		}
		for j, pt := range p.Points {
			if math.IsNaN(pt.X) || math.IsInf(pt.X, 0) || math.IsNaN(pt.Y) || math.IsInf(pt.Y, 0) {
				return rverr.Frame(rverr.EMeta, frame, "primitives["+itoa(i)+"].points["+itoa(j)+"]", "point coordinate is not finite")
			}
		}
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
