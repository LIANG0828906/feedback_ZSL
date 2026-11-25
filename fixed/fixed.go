package fixed

import (
	"math/big"

	"alertkernel/errs"
)

const (
	OverflowError    = "error"
	OverflowSaturate = "saturate"
)

type Value struct {
	Raw   int64 `json:"raw"`
	Scale int64 `json:"scale"`
	Bits  int   `json:"bits"`
}

func Bounds(bits int) (min, max *big.Int, err error) {
	if bits != 8 && bits != 16 && bits != 32 && bits != 64 {
		return nil, nil, errs.New(errs.EValidation, "numeric.bits", "unsupported fixed width %d", bits)
	}
	max = new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	max.Sub(max, big.NewInt(1))
	min = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), uint(bits-1)))
	return min, max, nil
}

func CheckRaw(raw int64, bits int, path string) error {
	min, max, err := Bounds(bits)
	if err != nil {
		return err
	}
	v := big.NewInt(raw)
	if v.Cmp(min) < 0 || v.Cmp(max) > 0 {
		return errs.New(errs.EFixedOverflow, path, "raw value %d exceeds declared %d-bit range [%s,%s]", raw, bits, min.String(), max.String())
	}
	return nil
}

func Add(a, b Value, policy, path string) (Value, bool, error) {
	if a.Scale != b.Scale {
		return Value{}, false, errs.New(errs.EValidation, path, "fixed scales differ: %d != %d", a.Scale, b.Scale)
	}
	min, max, err := Bounds(a.Bits)
	if err != nil {
		return Value{}, false, err
	}
	sum := new(big.Int).Add(big.NewInt(a.Raw), big.NewInt(b.Raw))
	overflowed := sum.Cmp(min) < 0 || sum.Cmp(max) > 0
	if !overflowed {
		return Value{Raw: sum.Int64(), Scale: a.Scale, Bits: a.Bits}, false, nil
	}
	if policy == OverflowSaturate {
		out := Value{Scale: a.Scale, Bits: a.Bits}
		if sum.Cmp(max) > 0 {
			out.Raw = max.Int64()
		} else {
			out.Raw = min.Int64()
		}
		return out, true, nil
	}
	return Value{}, true, errs.New(errs.EFixedOverflow, path, "fixed addition overflow at %d bits: %s", a.Bits, sum.String())
}

func Compare(raw int64, op string, threshold int64) (bool, error) {
	switch op {
	case ">", "gt":
		return raw > threshold, nil
	case ">=", "gte":
		return raw >= threshold, nil
	case "<", "lt":
		return raw < threshold, nil
	case "<=", "lte":
		return raw <= threshold, nil
	case "==", "=", "eq":
		return raw == threshold, nil
	case "!=", "ne":
		return raw != threshold, nil
	default:
		return false, errs.New(errs.EPredicateMalformed, "predicate.op", "unsupported operator %q", op)
	}
}
