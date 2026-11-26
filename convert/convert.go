package convert

import (
	"iconflow/errs"
	"iconflow/model"
	"math"
)

type Trace = model.ConversionRecord

func safeMul(a, b int64, field string) (int64, *errs.Error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, errs.Ovf(field, a, b, "integer multiplication overflow")
	}
	return a * b, nil
}

func safeAdd(a, b int64, field string) (int64, *errs.Error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, errs.Ovf(field, a, b, "integer addition overflow")
	}
	return a + b, nil
}

func DivMod(v, d int64, rounding, field string) (int64, int64, Trace, *errs.Error) {
	if d <= 0 {
		return 0, 0, Trace{}, errs.New(errs.ESchema, field, "divisor must be positive, got %d", d)
	}
	q := v / d
	r := v % d
	truncated := false
	switch rounding {
	case model.RoundCeil:
		if r != 0 && v >= 0 {
			nq, err := safeAdd(q, 1, field)
			if err != nil {
				return 0, 0, Trace{}, err
			}
			q = nq
			truncated = true
		}
	case model.RoundNearest:
		if r*2 >= d {
			nq, err := safeAdd(q, 1, field)
			if err != nil {
				return 0, 0, Trace{}, err
			}
			q = nq
			truncated = true
		}
	case "", model.RoundFloor:
		if r != 0 {
			truncated = true
		}
	default:
		return 0, 0, Trace{}, errs.Schema(field, "unsupported rounding %q", rounding)
	}
	trace := Trace{
		Field: field, Operation: "divmod", OperandA: v, OperandB: d,
		Quotient: q, Remainder: r, Rounding: rounding,
	}
	if truncated {
		trace.TruncatedAt = field
		trace.NewValue = q
		trace.OldValue = v
	}
	return q, r, trace, nil
}

func PermilleToPermyriad(v int, field string) (int, Trace, *errs.Error) {
	scaled, err := safeMul(int64(v), 10, field)
	if err != nil {
		return 0, Trace{}, err
	}
	return int(scaled), Trace{
		Field: field, Operation: "permille_to_permyriad",
		OperandA: v, OperandB: 10, NewValue: int(scaled),
	}, nil
}

func PermyriadToPermille(v int, rounding, field string) (int, Trace, *errs.Error) {
	q, _, trace, err := DivMod(int64(v), 10, rounding, field)
	if err != nil {
		return 0, Trace{}, err
	}
	trace.Operation = "permyriad_to_permille"
	return int(q), trace, nil
}

func BlockOrigin(index, cols, blockWidth, blockHeight int, field string) (originX, originY, tx, ty int, traces []Trace, err *errs.Error) {
	if cols <= 0 {
		return 0, 0, 0, 0, nil, errs.Schema(field+".columns", "columns must be positive")
	}
	if index < 0 {
		return 0, 0, 0, 0, nil, errs.Schema(field+".index", "block index must be non-negative")
	}
	q, r, tr, derr := DivMod(int64(index), int64(cols), model.RoundFloor, field+".index_to_row")
	if derr != nil {
		return 0, 0, 0, 0, nil, derr
	}
	ty, tx = int(q), int(r)
	traces = append(traces, tr)

	x64, oerr := safeMul(int64(tx), int64(blockWidth), field+".origin_x")
	if oerr != nil {
		return 0, 0, 0, 0, nil, oerr
	}
	y64, oerr := safeMul(int64(ty), int64(blockHeight), field+".origin_y")
	if oerr != nil {
		return 0, 0, 0, 0, nil, oerr
	}
	return int(x64), int(y64), tx, ty, traces, nil
}

func AbsoluteCoordinate(origin, local int, field string) (int, *errs.Error) {
	v, err := safeAdd(int64(origin), int64(local), field)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

func QuantizeCoordinate(absolute, step int, rounding, field string) (index int, residual int, trace Trace, err *errs.Error) {
	q, r, tr, derr := DivMod(int64(absolute), int64(step), rounding, field)
	if derr != nil {
		return 0, 0, Trace{}, derr
	}
	index = int(q)
	residual = int(r)
	return index, residual, tr, nil
}

func DequantizeCoordinate(index, step int, field string) (int, *errs.Error) {
	v, err := safeMul(int64(index), int64(step), field)
	return int(v), err
}

func ChannelOffset(layout string, x, y, width, height, channelIndex, channelCount int, field string) (int, Trace, *errs.Error) {
	rowBase, oerr := safeMul(int64(y), int64(width), field+".row_base")
	if oerr != nil {
		return 0, Trace{}, oerr
	}
	pixelIndex64, err := safeAdd(rowBase, int64(x), field+".pixel_index")
	if err != nil {
		return 0, Trace{}, err
	}
	pixelCount, err := safeMul(int64(width), int64(height), field+".pixel_count")
	if err != nil {
		return 0, Trace{}, err
	}
	var offset int64
	var op string
	switch layout {
	case model.LayoutFlat:
		base, merr := safeMul(pixelIndex64, int64(channelCount), field+".flat_base")
		if merr != nil {
			return 0, Trace{}, merr
		}
		offset, err = safeAdd(base, int64(channelIndex), field+".flat_offset")
		op = "flat_offset=(y*width+x)*channels+channel"
	case model.LayoutGrouped:
		base, merr := safeMul(int64(channelIndex), pixelCount, field+".group_base")
		if merr != nil {
			return 0, Trace{}, merr
		}
		offset, err = safeAdd(base, pixelIndex64, field+".group_offset")
		op = "grouped_offset=channel*width*height+y*width+x"
	default:
		return 0, Trace{}, errs.Schema(field, "unknown channel layout %q", layout)
	}
	if err != nil {
		return 0, Trace{}, err
	}
	return int(offset), Trace{
		Field: field, Operation: op, OperandA: pixelIndex64, OperandB: channelIndex,
		NewValue: int(offset),
	}, nil
}
func RemainderWithin(v int, declared int64) bool {
	if declared < 0 {
		declared = -declared
	}
	diff := int64(v)
	if diff < 0 {
		diff = -diff
	}
	return diff <= declared
}
