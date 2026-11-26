package lightprobe

import (
	"math"
	"math/big"
)

type WeightTerm struct {
	ProbeID          string  `json:"probe_id"`
	IJK              [3]int  `json:"ijk"`
	ExactWeight      float64 `json:"exact_weight"`
	WeightFixed      int64   `json:"weight_fixed"`
	NormalizedWeight float64 `json:"normalized_weight"`
}

type Interpolation struct {
	RoomID             string       `json:"room_id"`
	ClampedPosition    Vec3         `json:"clamped_position"`
	Terms              []WeightTerm `json:"terms"`
	WeightSumFixed     int64        `json:"weight_sum_fixed"`
	NormalizationError float64      `json:"normalization_error"`
	NormalizedSumError float64      `json:"normalized_sum_error"`
	RadianceFixed      [3]int64     `json:"radiance_fixed"`
	Radiance           [3]float64   `json:"radiance"`
}

type QueryResult struct {
	Query                Query          `json:"query"`
	Primary              Interpolation  `json:"primary"`
	Neighbor             *Interpolation `json:"neighbor,omitempty"`
	BlendRatioExact      float64        `json:"blend_ratio_exact"`
	BlendRatioQuantized  float64        `json:"blend_ratio_quantized"`
	BlendWeightFixed     int64          `json:"blend_weight_fixed"`
	BlendTruncationError float64        `json:"blend_truncation_error"`
	RadianceFixed        [3]int64       `json:"radiance_fixed"`
	Radiance             [3]float64     `json:"radiance"`
	Report               QueryReport    `json:"report"`
	Uncertain            bool           `json:"uncertain"`
	Lower                [3]float64     `json:"lower"`
	Upper                [3]float64     `json:"upper_bound"`
	WithinTolerance      bool           `json:"within_tolerance"`
	CacheKey             string         `json:"cache_key,omitempty"`
}

func InterpolateQuery(scene *Scene, q Query, results map[string]ProbeResult, params Params) (QueryResult, error) {
	room := scene.Room(q.PrimaryRoomID)
	if room == nil {
		return QueryResult{}, domainError(EVal, q.ID, "primary_room_id", "unknown room")
	}
	index := buildProbeIndex(scene)
	primary, err := interpolateRoom(scene, *room, q.Position, results, params, index)
	if err != nil {
		return QueryResult{}, err
	}
	qr := QueryResult{Query: q, Primary: primary, BlendRatioExact: q.BlendRatioExact}

	primaryFixed := primary.RadianceFixed
	finalFixed := primaryFixed
	if q.NeighborRoomID != "" {
		nr := scene.Room(q.NeighborRoomID)
		if nr == nil {
			return QueryResult{}, domainError(EVal, q.ID, "neighbor_room_id", "unknown room")
		}
		neighbor, err := interpolateRoom(scene, *nr, q.NeighborPosition, results, params, index)
		if err != nil {
			return QueryResult{}, err
		}
		qr.Neighbor = &neighbor
		betaFixed := int64(math.Floor(q.BlendRatioExact * float64(params.BlendScale)))
		if betaFixed < 0 {
			betaFixed = 0
		}
		if betaFixed > params.BlendScale {
			betaFixed = params.BlendScale
		}
		qr.BlendWeightFixed = betaFixed
		qr.BlendRatioQuantized = float64(betaFixed) / float64(params.BlendScale)
		qr.BlendTruncationError = math.Abs(q.BlendRatioExact - qr.BlendRatioQuantized)

		for ch := 0; ch < 3; ch++ {
			num := big.NewInt(params.BlendScale - betaFixed)
			num.Mul(num, big.NewInt(primary.RadianceFixed[ch]))
			right := big.NewInt(betaFixed)
			right.Mul(right, big.NewInt(neighbor.RadianceFixed[ch]))
			num.Add(num, right)
			out, rem := new(big.Int), new(big.Int)
			out.QuoRem(num, big.NewInt(params.BlendScale), rem)
			finalFixed[ch] = out.Int64()
		}
	}
	qr.RadianceFixed = finalFixed
	for ch := 0; ch < 3; ch++ {
		qr.Radiance[ch] = float64(finalFixed[ch]) / float64(params.FixedScale)
	}
	return qr, nil
}

type probeIndex map[string]string

func buildProbeIndex(s *Scene) probeIndex {
	idx := probeIndex{}
	for _, p := range s.Probes {
		idx[gridKey(p.RoomID, p.IJK)] = p.ID
	}
	return idx
}

func gridKey(roomID string, ijk [3]int) string {
	return roomID + "|" + itoa(ijk[0]) + "|" + itoa(ijk[1]) + "|" + itoa(ijk[2])
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	pos := len(b)
	for v > 0 {
		pos--
		b[pos] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func interpolateRoom(scene *Scene, room Room, pos Vec3, results map[string]ProbeResult, params Params, idx probeIndex) (Interpolation, error) {
	clamped := ClampToGrid(room, pos)
	first, _ := gridExtent(room)
	gx := (clamped.X - first.X) / room.Step.X
	gy := (clamped.Y - first.Y) / room.Step.Y
	gz := (clamped.Z - first.Z) / room.Step.Z
	i0 := clampIndex(int(math.Floor(gx)), room.Resolution[0]-1)
	j0 := clampIndex(int(math.Floor(gy)), room.Resolution[1]-1)
	k0 := clampIndex(int(math.Floor(gz)), room.Resolution[2]-1)
	tx := math.Max(0, math.Min(1, gx-float64(i0)))
	ty := math.Max(0, math.Min(1, gy-float64(j0)))
	tz := math.Max(0, math.Min(1, gz-float64(k0)))
	if i0 == room.Resolution[0]-1 {
		i0--
		tx = 1
	}
	if j0 == room.Resolution[1]-1 {
		j0--
		ty = 1
	}
	if k0 == room.Resolution[2]-1 {
		k0--
		tz = 1
	}

	type corner struct {
		ijk    [3]int
		weight float64
	}
	corners := []corner{
		{[3]int{i0, j0, k0}, (1 - tx) * (1 - ty) * (1 - tz)},
		{[3]int{i0 + 1, j0, k0}, tx * (1 - ty) * (1 - tz)},
		{[3]int{i0, j0 + 1, k0}, (1 - tx) * ty * (1 - tz)},
		{[3]int{i0 + 1, j0 + 1, k0}, tx * ty * (1 - tz)},
		{[3]int{i0, j0, k0 + 1}, (1 - tx) * (1 - ty) * tz},
		{[3]int{i0 + 1, j0, k0 + 1}, tx * (1 - ty) * tz},
		{[3]int{i0, j0 + 1, k0 + 1}, (1 - tx) * ty * tz},
		{[3]int{i0 + 1, j0 + 1, k0 + 1}, tx * ty * tz},
	}

	out := Interpolation{RoomID: room.ID, ClampedPosition: clamped}
	var sumFixed int64
	for _, cn := range corners {
		pid := idx[gridKey(room.ID, cn.ijk)]
		if pid == "" {
			return out, domainError(EVal, room.ID, "grid_index", "missing probe at %v", cn.ijk)
		}
		if _, ok := results[pid]; !ok {
			return out, domainError(EVal, pid, "reconstruction", "missing probe result")
		}
		wf := int64(math.Floor(cn.weight * float64(params.WeightScale)))
		if wf < 0 {
			wf = 0
		}
		if wf > params.WeightScale {
			wf = params.WeightScale
		}
		sumFixed += wf
		out.Terms = append(out.Terms, WeightTerm{
			ProbeID: pid, IJK: cn.ijk, ExactWeight: cn.weight, WeightFixed: wf,
		})
	}
	if sumFixed <= 0 {
		return out, domainError(EVal, room.ID, "interpolation_weights", "fixed weight sum is zero")
	}
	out.WeightSumFixed = sumFixed
	out.NormalizationError = math.Abs(1 - float64(sumFixed)/float64(params.WeightScale))
	normalizedSum := 0.0
	for i := range out.Terms {
		out.Terms[i].NormalizedWeight = float64(out.Terms[i].WeightFixed) / float64(sumFixed)
		normalizedSum += out.Terms[i].NormalizedWeight
	}
	out.NormalizedSumError = math.Abs(normalizedSum - 1)

	for ch := 0; ch < 3; ch++ {
		num := new(big.Int)
		for _, term := range out.Terms {
			r := results[term.ProbeID]
			part := big.NewInt(term.WeightFixed)
			part.Mul(part, big.NewInt(r.RadianceFixed[ch]))
			num.Add(num, part)
		}
		val, _ := new(big.Int), new(big.Int)
		val.QuoRem(num, big.NewInt(sumFixed), new(big.Int))
		out.RadianceFixed[ch] = val.Int64()
		out.Radiance[ch] = float64(out.RadianceFixed[ch]) / float64(params.FixedScale)
	}
	return out, nil
}

func clampIndex(v, max int) int {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}
