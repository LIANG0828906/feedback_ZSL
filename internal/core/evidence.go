package core

const (
	EvidenceWatermark = "watermark"
	EvidenceResidual  = "residual"
	EvidenceNoise     = "noise"
	EvidenceVerdict   = "verdict"
)

var EvidenceTypes = []string{
	EvidenceWatermark,
	EvidenceResidual,
	EvidenceNoise,
	EvidenceVerdict,
}

func CanonicalEvidenceOrder(frameCount int) []struct {
	Frame int
	Type  string
} {
	order := make([]struct {
		Frame int
		Type  string
	}, 0, frameCount*4)
	for frame := 0; frame < frameCount; frame++ {
		for _, evidenceType := range EvidenceTypes {
			order = append(order, struct {
				Frame int
				Type  string
			}{Frame: frame, Type: evidenceType})
		}
	}
	return order
}
