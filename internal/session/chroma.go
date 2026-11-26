package session

import (
	"strings"

	"mirrorcast/internal/bizerror"
)

func ParseChroma(raw string) (ChromaFormat, ChromaInfo, error) {
	normalized := strings.ToUpper(strings.TrimSpace(raw))
	normalized = strings.NewReplacer("PLANAR_", "", "CHROMA_", "", "YUV", "", "P", "", "_", "").Replace(normalized)
	switch normalized {
	case "444":
		return Chroma444, ChromaInfo{
			Format: Chroma444, PlaneEnum: "PLANAR_444", BitsPerPixel: 24,
			LumaBitsPerPixel: 8, ChromaBitsPerPixel: 16, BitDepth: 8,
			MinUnitWidth: 1, MinUnitHeight: 1, WidthDivisor: 1, HeightDivisor: 1,
		}, nil
	case "422":
		return Chroma422, ChromaInfo{
			Format: Chroma422, PlaneEnum: "PLANAR_422", BitsPerPixel: 16,
			LumaBitsPerPixel: 8, ChromaBitsPerPixel: 8, BitDepth: 8,
			MinUnitWidth: 2, MinUnitHeight: 1, WidthDivisor: 2, HeightDivisor: 1,
		}, nil
	case "420":
		return Chroma420, ChromaInfo{
			Format: Chroma420, PlaneEnum: "PLANAR_420", BitsPerPixel: 12,
			LumaBitsPerPixel: 8, ChromaBitsPerPixel: 4, BitDepth: 8,
			MinUnitWidth: 2, MinUnitHeight: 2, WidthDivisor: 2, HeightDivisor: 2,
		}, nil
	default:
		return "", ChromaInfo{}, bizerror.Newf(bizerror.EChromaEnumInvalid, "unsupported planar chroma enum %q", raw).
			WithDetail("allowed", []string{"PLANAR_444", "PLANAR_422", "PLANAR_420"})
	}
}

func ChromaFromBits(bitsPerPixel int64) (ChromaFormat, ChromaInfo, error) {
	switch bitsPerPixel {
	case 24:
		return ParseChroma("444")
	case 16:
		return ParseChroma("422")
	case 12:
		return ParseChroma("420")
	default:
		return "", ChromaInfo{}, bizerror.Newf(bizerror.EChromaBitsInvalid, "cannot derive planar chroma from %d bits/pixel", bitsPerPixel).
			WithDetail("allowed_bits_per_pixel", []int64{12, 16, 24})
	}
}
