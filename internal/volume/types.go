package volume

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"sort"

	"volumediag/internal/model"
)

const AlgorithmVersion = "screened-volume-ray-march/v1"

type BlockKey struct {
	CameraIndex int    `json:"camera_index"`
	CameraID    string `json:"camera_id"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
}

func (k BlockKey) Less(o BlockKey) bool {
	if k.CameraIndex != o.CameraIndex {
		return k.CameraIndex < o.CameraIndex
	}
	if k.Y != o.Y {
		return k.Y < o.Y
	}
	return k.X < o.X
}

type PixelResult struct {
	X             int         `json:"x"`
	Y             int         `json:"y"`
	Transmittance model.Color `json:"transmittance"`
	Scatter       model.Color `json:"scatter"`
	Volume        model.Color `json:"volume"`
	UsedLights    []string    `json:"used_lights"`
}

type BlockStats struct {
	MeanTransmittance model.Color `json:"mean_transmittance"`
	MeanScatter       model.Color `json:"mean_scatter"`
	MaxScatter        model.Color `json:"max_scatter"`
	SamplesPerPixel   int         `json:"samples_per_pixel"`
	PixelCount        int         `json:"pixel_count"`
}

type BlockResult struct {
	Key              BlockKey          `json:"key"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	Pixels           []PixelResult     `json:"pixels"`
	Composite        []model.Color     `json:"composite"`
	Stats            BlockStats        `json:"stats"`
	UsedLights       []string          `json:"used_lights"`
	UsedOccluders    []string          `json:"used_occluders"`
	ActualStep       float64           `json:"actual_step"`
	Jitter           string            `json:"jitter"`
	JitterSamples    int               `json:"jitter_samples"`
	ParamsVersion    string            `json:"params_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	MediumVersion    string            `json:"medium_version"`
	GeometryVersion  string            `json:"geometry_version"`
	LightVersions    map[string]string `json:"light_versions"`
	Digest           string            `json:"digest"`
}

type RayResult struct {
	Transmittance model.Color `json:"transmittance"`
	Scatter       model.Color `json:"scatter"`
	Samples       int         `json:"samples"`
	ActualStep    float64     `json:"actual_step"`
	UsedLights    []string    `json:"used_lights"`
}

type Quality struct {
	RequestedStep      float64 `json:"requested_step"`
	ActualStep         float64 `json:"actual_step"`
	StepsPerRay        int     `json:"steps_per_ray"`
	JitterSamples      int     `json:"jitter_samples"`
	SamplesPerPixel    int     `json:"samples_per_pixel"`
	LightCount         int     `json:"light_count"`
	SigmaTMin          float64 `json:"sigma_t_min"`
	ErrorBoundPerBlock float64 `json:"error_bound_per_block"`
	ErrorBoundPerPixel float64 `json:"error_bound_per_pixel"`
	Formula            string  `json:"formula"`
}

func (b *BlockResult) ComputeDigest() string {
	h := sha256.New()
	writeString := func(s string) {
		_ = binary.Write(h, binary.LittleEndian, uint32(len(s)))
		_, _ = h.Write([]byte(s))
	}
	writeFloat := func(v float64) {
		_ = binary.Write(h, binary.LittleEndian, math.Float64bits(v))
	}
	writeInt := func(v int) { _ = binary.Write(h, binary.LittleEndian, int64(v)) }
	writeString(b.Key.CameraID)
	writeInt(b.Key.CameraIndex)
	writeInt(b.Key.X)
	writeInt(b.Key.Y)
	writeInt(b.Width)
	writeInt(b.Height)
	writeFloat(b.ActualStep)
	writeString(b.Jitter)
	writeInt(b.JitterSamples)
	writeString(b.ParamsVersion)
	writeString(b.AlgorithmVersion)
	writeString(b.MediumVersion)
	writeString(b.GeometryVersion)
	lightIDs := make([]string, 0, len(b.LightVersions))
	for id := range b.LightVersions {
		lightIDs = append(lightIDs, id)
	}
	sort.Strings(lightIDs)
	for _, id := range lightIDs {
		writeString(id)
		writeString(b.LightVersions[id])
	}
	for _, p := range b.Pixels {
		writeInt(p.X)
		writeInt(p.Y)
		for c := 0; c < 3; c++ {
			writeFloat(p.Transmittance[c])
			writeFloat(p.Scatter[c])
			writeFloat(p.Volume[c])
		}
	}
	for _, c := range b.Composite {
		for i := 0; i < 3; i++ {
			writeFloat(c[i])
		}
	}
	for c := 0; c < 3; c++ {
		writeFloat(b.Stats.MeanTransmittance[c])
		writeFloat(b.Stats.MeanScatter[c])
		writeFloat(b.Stats.MaxScatter[c])
	}
	writeInt(b.Stats.SamplesPerPixel)
	writeInt(b.Stats.PixelCount)
	for _, id := range b.UsedLights {
		writeString(id)
	}
	for _, id := range b.UsedOccluders {
		writeString(id)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func SortStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
