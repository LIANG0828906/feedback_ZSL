package lightprobe

import (
	"math"
	"math/rand"
	"strconv"
	"strings"
)

func GenerateScene(seed int64, params Params) (*Scene, error) {
	if params.FixedScale == 0 {
		params = DefaultParams()
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(seed))
	scene := &Scene{
		Seed:   seed,
		Params: params,
		Screen: Screen{Width: 16, Height: 8},
	}

	roomSpecs := []struct {
		id  string
		min Vec3
		max Vec3
	}{
		{"room-a", Vec3{-9, -2, -2}, Vec3{-3, 2, 2}},
		{"room-b", Vec3{-3, -2, -2}, Vec3{3, 2, 2}},
		{"room-c", Vec3{3, -2, -2}, Vec3{9, 2, 2}},
	}
	resolution := [3]int{5, 3, 3}
	offset := Vec3{0.5, 0.5, 0.5}

	for _, spec := range roomSpecs {
		room := Room{
			ID:         spec.id,
			Bounds:     Bounds{Min: spec.min, Max: spec.max},
			Resolution: resolution,
			Offset:     offset,
		}
		room.Step = Vec3{
			(spec.max.X - spec.min.X) / float64(resolution[0]),
			(spec.max.Y - spec.min.Y) / float64(resolution[1]),
			(spec.max.Z - spec.min.Z) / float64(resolution[2]),
		}
		room.ResolutionVersion = "res-" + strings.ReplaceAll(spec.id, "room-", "") + "-5x3x3-v1"
		for c := 0; c < 3; c++ {
			base := 0.42 + float64(c)*0.07
			room.FieldCoefficients[c] = [8]float64{
				base,
				(0.075 + rng.Float64()*0.025) * deterministicSign(rng),
				(0.050 + rng.Float64()*0.020) * deterministicSign(rng),
				(0.040 + rng.Float64()*0.018) * deterministicSign(rng),
				(0.026 + rng.Float64()*0.012) * deterministicSign(rng),
				(0.022 + rng.Float64()*0.010) * deterministicSign(rng),
				(0.020 + rng.Float64()*0.010) * deterministicSign(rng),
				(0.014 + rng.Float64()*0.008) * deterministicSign(rng),
			}
		}

		points, err := PlanGrid(room.Bounds, room.Resolution, room.Offset)
		if err != nil {
			return nil, err
		}
		for _, gp := range points {
			probe := makeProbe(room, gp.IJK, gp.Position, rng, params)
			bundle, err := EncodeSampleBundle(SampleBundle{
				ProbeID:        probe.ID,
				RoomID:         probe.RoomID,
				Radiance:       probe.Radiance,
				RawSampleCount: probe.RawSampleCount,
				SampleCount:    probe.SampleCount,
				SHScale:        probe.SHScale,
				SH:             probe.SH,
				ContentHash:    probe.ContentHash,
			})
			if err != nil {
				return nil, err
			}
			probe.SampleBlob = bundle
			scene.Probes = append(scene.Probes, probe)
			room.ProbeIDs = append(room.ProbeIDs, probe.ID)
		}
		scene.Rooms = append(scene.Rooms, room)
	}

	scene.Frames = make([]CameraFrame, 6)
	for i := range scene.Frames {
		t := float64(i) / 5.0
		eyeX := -10.0 + 20.0*t
		scene.Frames[i] = CameraFrame{
			ID:     "cam-" + strconv.Itoa(i),
			Time:   float64(i) * 0.25,
			Eye:    Vec3{eyeX, 3.2, 4.4},
			Target: Vec3{eyeX * 0.72, 0, 0},
		}
	}

	for y := 0; y < scene.Screen.Height; y++ {
		for x := 0; x < scene.Screen.Width; x++ {
			pixelID := "px-" + twoDigits(x) + "-" + twoDigits(y)
			u := float64(x) / float64(scene.Screen.Width-1)
			v := float64(y) / float64(scene.Screen.Height-1)
			pos := Vec3{
				X: -8.6 + 17.2*u,
				Y: -1.65 + 3.3*v,
				Z: 0.35 * math.Sin((u*2.0-1.0)*math.Pi) * math.Cos((v*2.0-1.0)*math.Pi/2),
			}
			qid := "q-" + twoDigits(x) + "-" + twoDigits(y)
			scene.Pixels = append(scene.Pixels, Pixel{ID: pixelID, X: x, Y: y, QueryID: qid})
			q := scene.newQuery(qid, pixelID, float64(y*scene.Screen.Width+x), pos)
			scene.Queries = append(scene.Queries, q)
		}
	}

	// Explicit camera-path samples include exact shared-face crossings.
	pathPositions := []float64{-8.4, -6.0, -3.0, -2.72, 0.0, 2.72, 3.0, 6.0, 8.4}
	for i, x := range pathPositions {
		qid := "q-path-" + twoDigits(i)
		pixelID := "px-path-" + twoDigits(i)
		pos := Vec3{X: x, Y: 0.55 * math.Sin(float64(i)*0.9), Z: 0.25 * math.Cos(float64(i)*0.7)}
		q := scene.newQuery(qid, pixelID, 1000+float64(i), pos)
		scene.Queries = append(scene.Queries, q)
	}

	for i := 0; i < 12; i++ {
		q := scene.Queries[(i*7)%len(scene.Queries)]
		scene.Directives = append(scene.Directives, ResampleDirective{
			ID:           "dir-" + threeDigits(i),
			Time:         float64(i) * 0.125,
			QueryID:      q.ID,
			TargetRoomID: q.PrimaryRoomID,
			Position:     q.Position,
			Reason:       "deterministic-camera-resample",
		})
	}

	for _, q := range scene.Queries {
		scene.Truths = append(scene.Truths, TruthSample{
			QueryID:  q.ID,
			Position: q.Position,
			Expected: ExpectedRadiance(scene, q),
		})
	}

	return scene, nil
}

func deterministicSign(rng *rand.Rand) float64 {
	if rng.Intn(2) == 0 {
		return -1
	}
	return 1
}

func twoDigits(v int) string {
	if v < 10 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

func threeDigits(v int) string {
	if v < 10 {
		return "00" + strconv.Itoa(v)
	}
	if v < 100 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

type GridPoint struct {
	IJK      [3]int
	Position Vec3
}

func PlanGrid(b Bounds, res [3]int, offset Vec3) ([]GridPoint, error) {
	objectID := "grid:" + "plan"
	for axis, n := range res {
		if n < 2 {
			return nil, domainError(EVal, objectID, "resolution["+string(rune('x'+axis))+"]", "must be at least 2")
		}
	}
	if offset.X < 0 || offset.X > 1 || offset.Y < 0 || offset.Y > 1 || offset.Z < 0 || offset.Z > 1 {
		return nil, domainError(EVal, objectID, "grid_offset", "normalized offset must be in [0,1]")
	}
	size := b.Size()
	step := Vec3{size.X / float64(res[0]), size.Y / float64(res[1]), size.Z / float64(res[2])}
	var points []GridPoint
	for i := 0; i < res[0]; i++ {
		for j := 0; j < res[1]; j++ {
			for k := 0; k < res[2]; k++ {
				gp := GridPoint{
					IJK: [3]int{i, j, k},
					Position: Vec3{
						X: b.Min.X + (float64(i)+offset.X)*step.X,
						Y: b.Min.Y + (float64(j)+offset.Y)*step.Y,
						Z: b.Min.Z + (float64(k)+offset.Z)*step.Z,
					},
				}
				if !b.Contains(gp.Position) {
					return nil, domainError(EVal, objectID, "grid_point["+strconv.Itoa(i)+","+strconv.Itoa(j)+","+strconv.Itoa(k)+"]",
						"parameter injection produced point %v outside bounds %v", gp.Position, b)
				}
				points = append(points, gp)
			}
		}
	}
	return points, nil
}

func makeProbe(room Room, ijk [3]int, pos Vec3, rng *rand.Rand, params Params) Probe {
	id := "probe-" + room.ID + "-" + twoDigits(ijk[0]) + twoDigits(ijk[1]) + twoDigits(ijk[2])
	p := Probe{
		ID:             id,
		RoomID:         room.ID,
		IJK:            ijk,
		Position:       pos,
		Radiance:       EvalField(room, pos),
		RawSampleCount: 64 + rng.Float64()*48,
		SHScale:        params.FixedScale,
	}
	p.SampleCount = int(math.Round(p.RawSampleCount))
	directions := shDirections()
	for c := 0; c < 3; c++ {
		highMean := 0.0
		p.RawSH[c][0] = 0
		for k := 1; k < 9; k++ {
			p.RawSH[c][k] = (rng.Float64()*2 - 1) * 0.022
		}
		for _, d := range directions {
			basis := SHBasis(d)
			for k := 1; k < 9; k++ {
				highMean += p.RawSH[c][k] * basis[k]
			}
		}
		highMean /= float64(len(directions))
		basis0 := SHBasis(Vec3{0, 0, 1})[0]
		p.RawSH[c][0] = (p.Radiance[c] - highMean) / basis0
		for k := 0; k < 9; k++ {
			p.SH[c][k] = quantizeFixed(p.RawSH[c][k], params.FixedScale)
		}
	}
	p.ContentHash = HashProbeContent(p)
	return p
}

func EvalField(room Room, pos Vec3) [3]float64 {
	size := room.Bounds.Size()
	u := (pos.X - room.Bounds.Min.X) / size.X
	v := (pos.Y - room.Bounds.Min.Y) / size.Y
	w := (pos.Z - room.Bounds.Min.Z) / size.Z
	u = math.Max(0, math.Min(1, u))
	v = math.Max(0, math.Min(1, v))
	w = math.Max(0, math.Min(1, w))
	a, b, c := u-0.5, v-0.5, w-0.5
	terms := [8]float64{1, a, b, c, a * b, b * c, c * a, a * b * c}
	var out [3]float64
	for ch := 0; ch < 3; ch++ {
		v := 0.0
		for i := 0; i < 8; i++ {
			v += room.FieldCoefficients[ch][i] * terms[i]
		}
		out[ch] = math.Max(0, math.Min(1, v))
	}
	return out
}

func SHBasis(d Vec3) [9]float64 {
	x, y, z := d.X, d.Y, d.Z
	return [9]float64{
		0.28209479177387814,
		0.4886025119029199 * y,
		0.4886025119029199 * z,
		0.4886025119029199 * x,
		1.0925484305920792 * x * y,
		1.0925484305920792 * y * z,
		0.31539156525252005 * (3*z*z - 1),
		1.0925484305920792 * x * z,
		0.5462742152960396 * (x*x - y*y),
	}
}

func shDirections() []Vec3 {
	const n = 16
	dirs := make([]Vec3, n)
	golden := math.Pi * (3 - math.Sqrt(5))
	for i := 0; i < n; i++ {
		y := 1 - (float64(i)+0.5)*2/float64(n)
		r := math.Sqrt(math.Max(0, 1-y*y))
		theta := golden * float64(i)
		d := Vec3{X: math.Cos(theta) * r, Y: y, Z: math.Sin(theta) * r}
		dirs[i] = d
	}
	return dirs
}

func quantizeFixed(v float64, scale int64) int64 {
	return int64(math.Round(v * float64(scale)))
}

func (s *Scene) newQuery(id, pixelID string, frame float64, pos Vec3) Query {
	q := Query{
		ID:         id,
		PixelID:    pixelID,
		FrameIndex: int(frame) % len(s.Frames),
		Time:       frame * 0.02,
		Position:   pos,
	}
	room := RoomAt(s, pos)
	if room != nil {
		q.PrimaryRoomID = room.ID
	}
	attachBoundary(s, &q)
	return q
}

func RoomAt(s *Scene, pos Vec3) *Room {
	for i := range s.Rooms {
		if s.Rooms[i].Bounds.Contains(pos) {
			return &s.Rooms[i]
		}
	}
	return nil
}

func attachBoundary(s *Scene, q *Query) {
	room := s.Room(q.PrimaryRoomID)
	if room == nil {
		return
	}
	w := s.Params.BlendWidth
	type candidate struct {
		neighbor    *Room
		axis        int
		boundary    float64
		distance    float64
		neighborPos Vec3
	}
	var candidates []candidate

	if d := room.Bounds.Max.X - q.Position.X; d >= -1e-9 && d < w {
		for i := range s.Rooms {
			n := &s.Rooms[i]
			if n.ID != room.ID && math.Abs(n.Bounds.Min.X-room.Bounds.Max.X) < 1e-9 &&
				q.Position.Y >= n.Bounds.Min.Y && q.Position.Y <= n.Bounds.Max.Y &&
				q.Position.Z >= n.Bounds.Min.Z && q.Position.Z <= n.Bounds.Max.Z {
				candidates = append(candidates, candidate{
					neighbor: n, axis: 0, boundary: room.Bounds.Max.X, distance: math.Max(0, d),
					neighborPos: Vec3{room.Bounds.Max.X + math.Max(0, d), q.Position.Y, q.Position.Z},
				})
			}
		}
	}
	if d := q.Position.X - room.Bounds.Min.X; d >= -1e-9 && d < w {
		for i := range s.Rooms {
			n := &s.Rooms[i]
			if n.ID != room.ID && math.Abs(n.Bounds.Max.X-room.Bounds.Min.X) < 1e-9 &&
				q.Position.Y >= n.Bounds.Min.Y && q.Position.Y <= n.Bounds.Max.Y &&
				q.Position.Z >= n.Bounds.Min.Z && q.Position.Z <= n.Bounds.Max.Z {
				candidates = append(candidates, candidate{
					neighbor: n, axis: 0, boundary: room.Bounds.Min.X, distance: math.Max(0, d),
					neighborPos: Vec3{room.Bounds.Min.X - math.Max(0, d), q.Position.Y, q.Position.Z},
				})
			}
		}
	}
	if len(candidates) == 0 {
		return
	}
	best := candidates[0]
	for _, cand := range candidates[1:] {
		if cand.distance < best.distance {
			best = cand
		}
	}
	ratio := 0.5 * (1 - best.distance/w)
	if ratio <= 0 {
		return
	}
	q.NeighborRoomID = best.neighbor.ID
	q.BoundaryAxis = best.axis
	q.BoundaryValue = best.boundary
	q.DistanceToBoundary = best.distance
	q.BlendRatioExact = ratio
	q.NeighborPosition = best.neighborPos
}

func gridExtent(room Room) (Vec3, Vec3) {
	first := room.Bounds.Min.Add(room.Offset.Mul(0))
	first = Vec3{
		X: room.Bounds.Min.X + room.Offset.X*room.Step.X,
		Y: room.Bounds.Min.Y + room.Offset.Y*room.Step.Y,
		Z: room.Bounds.Min.Z + room.Offset.Z*room.Step.Z,
	}
	last := Vec3{
		X: room.Bounds.Min.X + (float64(room.Resolution[0]-1)+room.Offset.X)*room.Step.X,
		Y: room.Bounds.Min.Y + (float64(room.Resolution[1]-1)+room.Offset.Y)*room.Step.Y,
		Z: room.Bounds.Min.Z + (float64(room.Resolution[2]-1)+room.Offset.Z)*room.Step.Z,
	}
	return first, last
}

func ClampToGrid(room Room, pos Vec3) Vec3 {
	first, last := gridExtent(room)
	clampAxis := func(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
	return Vec3{X: clampAxis(pos.X, first.X, last.X), Y: clampAxis(pos.Y, first.Y, last.Y), Z: clampAxis(pos.Z, first.Z, last.Z)}
}

func ExpectedRadiance(s *Scene, q Query) [3]float64 {
	room := s.Room(q.PrimaryRoomID)
	if room == nil {
		return [3]float64{}
	}
	primary := EvalField(*room, ClampToGrid(*room, q.Position))
	if q.NeighborRoomID == "" {
		return primary
	}
	neighborRoom := s.Room(q.NeighborRoomID)
	if neighborRoom == nil {
		return primary
	}
	neighbor := EvalField(*neighborRoom, ClampToGrid(*neighborRoom, q.NeighborPosition))
	beta := q.BlendRatioExact
	var out [3]float64
	for c := 0; c < 3; c++ {
		out[c] = (1-beta)*primary[c] + beta*neighbor[c]
	}
	return out
}
