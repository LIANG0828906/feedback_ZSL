package engine

type weightTap struct {
	OffsetX int
	OffsetY int
	Weight  int64
}

func kernelWeights(kernel string, factorX, factorY int, scale int64) ([]weightTap, int64) {
	switch kernel {
	case "nearest":
		return []weightTap{{OffsetX: factorX / 2, OffsetY: factorY / 2, Weight: scale}}, 0
	case "box":
		taps := int64(factorX * factorY)
		w := scale / taps
		normResidual := scale - w*taps
		tapsList := make([]weightTap, 0, factorX*factorY)
		for y := 0; y < factorY; y++ {
			for x := 0; x < factorX; x++ {
				tapsList = append(tapsList, weightTap{OffsetX: x, OffsetY: y, Weight: w})
			}
		}
		return tapsList, normResidual
	default:
		return nil, 0
	}
}
