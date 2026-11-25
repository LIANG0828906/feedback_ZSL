package scene

import (
	"fmt"
	"math"
)

func ProbeID(roomID string, i, j, k int) string {
	return fmt.Sprintf("probe:%s:%d:%d:%d", roomID, i, j, k)
}

func ProbeStep(room Room) Vec3 {
	size := Vec3{
		room.Bounds.Max[0] - room.Bounds.Min[0],
		room.Bounds.Max[1] - room.Bounds.Min[1],
		room.Bounds.Max[2] - room.Bounds.Min[2],
	}
	res := [3]int{room.Resolution.X, room.Resolution.Y, room.Resolution.Z}
	var step Vec3
	for axis := 0; axis < 3; axis++ {
		denom := float64(res[axis] - 1)
		if denom > 0 {
			usable := size[axis] - 2*room.GridOffset[axis]
			if usable < 0 {
				usable = size[axis]
			}
			step[axis] = usable / denom
		}
	}
	return step
}

func ProbePosition(room Room, i, j, k int) Vec3 {
	idx := [3]int{i, j, k}
	step := ProbeStep(room)
	pos := room.Bounds.Min
	for axis, index := range idx {
		pos[axis] += room.GridOffset[axis] + float64(index)*step[axis]
	}
	return pos
}

func AnalyticRadiance(room Room, p Vec3) Vec3 {
	gradient := Vec3{0.035, 0.024, 0.018}
	var out Vec3
	for c := 0; c < 3; c++ {
		out[c] = room.AnalyticBias[c] +
			gradient[0]*p[0] +
			gradient[1]*p[1] +
			gradient[2]*p[2] +
			0.0025*float64(room.Index+1)*float64(c+1)
	}
	return out
}

func Shade(pixel Pixel, radiance Vec3) Vec3 {
	var out Vec3
	for c := 0; c < 3; c++ {
		out[c] = pixel.Ambient[c] + pixel.BaseColor[c]*radiance[c]*pixel.Exposure
		out[c] = math.Max(0, math.Min(1, out[c]))
	}
	return out
}

func (b AABB) Contains(p Vec3, eps float64) bool {
	for axis := 0; axis < 3; axis++ {
		if p[axis] < b.Min[axis]-eps || p[axis] > b.Max[axis]+eps {
			return false
		}
	}
	return true
}

func (b AABB) InteriorsOverlap(other AABB) bool {
	for axis := 0; axis < 3; axis++ {
		lo := math.Max(b.Min[axis], other.Min[axis])
		hi := math.Min(b.Max[axis], other.Max[axis])
		if hi <= lo+1e-12 {
			return false
		}
	}
	return true
}

func FindRoomAt(scene *Scene, p Vec3, eps float64) (Room, int, bool) {
	for i, room := range scene.Rooms {
		if room.Bounds.Contains(p, eps) {
			return room, i, true
		}
	}
	return Room{}, -1, false
}

type BoundarySample struct {
	Rule     BoundaryRule
	Owner    string
	Other    Room
	OtherAt  Vec3
	Distance float64
	Alpha    float64
}

func BoundarySampleAt(scene *Scene, ownerID string, p Vec3) (BoundarySample, bool) {
	const eps = 1e-8
	for _, rule := range scene.Boundaries {
		var otherID string
		var distance float64
		switch ownerID {
		case rule.RoomA:
			otherID = rule.RoomB
			distance = rule.Position - p[rule.Axis]
		case rule.RoomB:
			otherID = rule.RoomA
			distance = p[rule.Axis] - rule.Position
		default:
			continue
		}
		other, _, ok := scene.RoomByID(otherID)
		if !ok {
			continue
		}
		q := p
		q[rule.Axis] = rule.Position
		if !other.Bounds.Contains(q, eps) {
			continue
		}
		if distance < -eps || distance > rule.BlendWidth+eps {
			continue
		}
		distance = math.Max(0, distance)
		t := distance / rule.BlendWidth
		t = math.Max(0, math.Min(1, t))
		alpha := 0.5 * (1 - t)
		return BoundarySample{
			Rule: rule, Owner: ownerID, Other: other, OtherAt: q,
			Distance: distance, Alpha: alpha,
		}, true
	}
	return BoundarySample{}, false
}

func ApplyAnalyticBoundaryBlend(scene *Scene, owner Room, p Vec3) (Vec3, BoundarySample, bool) {
	sample, ok := BoundarySampleAt(scene, owner.ID, p)
	if !ok || sample.Alpha == 0 {
		return AnalyticRadiance(owner, p), sample, ok
	}
	ownerRadiance := AnalyticRadiance(owner, p)
	otherRadiance := AnalyticRadiance(sample.Other, sample.OtherAt)
	var out Vec3
	for c := 0; c < 3; c++ {
		out[c] = (1-sample.Alpha)*ownerRadiance[c] + sample.Alpha*otherRadiance[c]
	}
	return out, sample, true
}
