package slikernel

import "math/big"

// mulDivFloor computes floor(a*b/c) with a 128-bit intermediate. All inputs
// are non-negative. This keeps fixed-point ratios and nearest-rank quantiles
// exact regardless of event counts on 64-bit platforms.
func mulDivFloor(a, b, c int64) int64 {
	if c <= 0 || a <= 0 || b == 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	quotient := new(big.Int).Quo(product, big.NewInt(c))
	return quotient.Int64()
}

func mulDivRound(a, b, c int64) int64 {
	if c <= 0 || a <= 0 || b == 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	product.Add(product, big.NewInt(c/2))
	quotient := new(big.Int).Quo(product, big.NewInt(c))
	return quotient.Int64()
}

func mulDivCeil(a, b, c int64) int64 {
	if c <= 0 || a <= 0 || b == 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	product.Add(product, big.NewInt(c-1))
	quotient := new(big.Int).Quo(product, big.NewInt(c))
	return quotient.Int64()
}

// ratioPPM returns a rounded fixed-point ratio, saturated at 1,000,000 for
// values greater than one.
func ratioPPM(numerator, denominator int64) int64 {
	if denominator <= 0 {
		return PPM
	}
	if numerator <= 0 {
		return 0
	}
	if numerator >= denominator {
		return PPM
	}
	return mulDivRound(numerator, PPM, denominator)
}

// nearestRankQuantile implements the discrete nearest-rank definition. No
// interpolation or zero-fill is used, so gaps cannot shift the result.
func nearestRankQuantile(sortedSamples []int64, quantilePPM int64) int64 {
	if len(sortedSamples) == 0 {
		return 0
	}
	rank := mulDivCeil(int64(len(sortedSamples)), quantilePPM, PPM)
	if rank < 1 {
		rank = 1
	}
	if rank > int64(len(sortedSamples)) {
		rank = int64(len(sortedSamples))
	}
	return sortedSamples[rank-1]
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
