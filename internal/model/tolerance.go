package model

type FixedTolerance struct {
	LSNAbs uint64 `json:"lsn_abs"`
	Offset uint64 `json:"offset"`
	Index  int    `json:"index"`
	TimeMS int64  `json:"time_ms"`
	Tick   int64  `json:"tick"`
}

func ExactTolerance() FixedTolerance {
	return FixedTolerance{}
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func PointWithinTolerance(got, want Point, tol FixedTolerance) bool {
	lsnDiff := got.LSN - want.LSN
	if got.LSN < want.LSN {
		lsnDiff = want.LSN - got.LSN
	}
	offDiff := got.Offset - want.Offset
	if got.Offset < want.Offset {
		offDiff = want.Offset - got.Offset
	}
	return lsnDiff <= tol.LSNAbs &&
		offDiff <= tol.Offset &&
		absInt(got.Index-want.Index) <= tol.Index &&
		absInt64(got.TimeMS-want.TimeMS) <= tol.TimeMS &&
		absInt64(got.Tick-want.Tick) <= tol.Tick
}

func RangeWithinTolerance(got, want ReplayRange, tol FixedTolerance) bool {
	if got.SegmentID != want.SegmentID || got.Replay != want.Replay {
		return false
	}
	if !PointWithinTolerance(got.Start, want.Start, tol) || !PointWithinTolerance(got.End, want.End, tol) {
		return false
	}
	lsnDiff := got.StartLSN - want.StartLSN
	if got.StartLSN < want.StartLSN {
		lsnDiff = want.StartLSN - got.StartLSN
	}
	endDiff := got.EndLSN - want.EndLSN
	if got.EndLSN < want.EndLSN {
		endDiff = want.EndLSN - got.EndLSN
	}
	return lsnDiff <= tol.LSNAbs && endDiff <= tol.LSNAbs && absInt(got.StartIndex-want.StartIndex) <= tol.Index && absInt(got.EndIndex-want.EndIndex) <= tol.Index
}
