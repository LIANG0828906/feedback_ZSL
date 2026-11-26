package lightprobe

import (
	"math"
)

func ApplyRadianceEdit(scene *Scene, probeID string, channel int, value float64) (*Probe, error) {
	if channel < 0 || channel > 2 {
		return nil, domainError(EVal, probeID, "channel", "channel must be 0..2")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return nil, domainError(EVal, probeID, "radiance", "edited radiance %.6f must be finite and within [0,1]", value)
	}
	p := scene.ProbeByID(probeID)
	if p == nil {
		return nil, domainError(ESchema, probeID, "id", "probe not found")
	}
	p.Radiance[channel] = value
	p.ContentHash = HashProbeContent(*p)
	bundle, err := EncodeSampleBundle(SampleBundle{
		ProbeID: p.ID, RoomID: p.RoomID, Radiance: p.Radiance,
		RawSampleCount: p.RawSampleCount, SampleCount: p.SampleCount,
		SHScale: p.SHScale, SH: p.SH, ContentHash: p.ContentHash,
	})
	if err != nil {
		return nil, err
	}
	p.SampleBlob = bundle
	return p, nil
}

func CorruptSample(scene *Scene, probeID string) error {
	p := scene.ProbeByID(probeID)
	if p == nil {
		return domainError(ESchema, probeID, "id", "probe not found")
	}
	if len(p.SampleBlob) == 0 {
		return domainError(EDecode, probeID, "sample_blob", "sample blob is empty")
	}
	p.SampleBlob = append(append([]byte(nil), p.SampleBlob...), 0xff)
	return nil
}

func ValidateGridParameterInjection(room Room, res [3]int, offset Vec3) error {
	_, err := PlanGrid(room.Bounds, res, offset)
	return err
}
