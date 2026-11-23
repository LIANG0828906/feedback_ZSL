package mesh

import (
	"math"
	"sort"
)

type aabb struct {
	min Vec3
	max Vec3
}

func faceAABB(m Mesh, fi int) aabb {
	f := m.Faces[fi]
	a, b, c := m.Vertices[f.A], m.Vertices[f.B], m.Vertices[f.C]
	bx := aabb{min: a, max: a}
	for _, v := range []Vec3{b, c} {
		if v.X < bx.min.X {
			bx.min.X = v.X
		}
		if v.Y < bx.min.Y {
			bx.min.Y = v.Y
		}
		if v.Z < bx.min.Z {
			bx.min.Z = v.Z
		}
		if v.X > bx.max.X {
			bx.max.X = v.X
		}
		if v.Y > bx.max.Y {
			bx.max.Y = v.Y
		}
		if v.Z > bx.max.Z {
			bx.max.Z = v.Z
		}
	}
	return bx
}

func overlaps(a, b aabb, eps float64) bool {
	return a.min.X <= b.max.X+eps && a.max.X+eps >= b.min.X &&
		a.min.Y <= b.max.Y+eps && a.max.Y+eps >= b.min.Y &&
		a.min.Z <= b.max.Z+eps && a.max.Z+eps >= b.min.Z
}

func expand(b aabb, eps float64) aabb {
	return aabb{
		min: Vec3{X: b.min.X - eps, Y: b.min.Y - eps, Z: b.min.Z - eps},
		max: Vec3{X: b.max.X + eps, Y: b.max.Y + eps, Z: b.max.Z + eps},
	}
}

type bvhNode struct {
	box         aabb
	left, right int
	face        int
	start, end  int
}

type bvh struct {
	nodes []bvhNode
	items []int
}

func buildBVH(m Mesh) bvh {
	n := len(m.Faces)
	items := make([]int, n)
	boxes := make([]aabb, n)
	for i := range items {
		items[i] = i
		boxes[i] = faceAABB(m, i)
	}
	tr := bvh{nodes: make([]bvhNode, 0, n*2), items: items}
	var build func(start, end int) int
	build = func(start, end int) int {
		node := bvhNode{start: start, end: end, face: -1, left: -1, right: -1}
		box := boxes[tr.items[start]]
		for _, fi := range tr.items[start+1 : end] {
			b := boxes[fi]
			if b.min.X < box.min.X {
				box.min.X = b.min.X
			}
			if b.min.Y < box.min.Y {
				box.min.Y = b.min.Y
			}
			if b.min.Z < box.min.Z {
				box.min.Z = b.min.Z
			}
			if b.max.X > box.max.X {
				box.max.X = b.max.X
			}
			if b.max.Y > box.max.Y {
				box.max.Y = b.max.Y
			}
			if b.max.Z > box.max.Z {
				box.max.Z = b.max.Z
			}
		}
		node.box = box
		idx := len(tr.nodes)
		tr.nodes = append(tr.nodes, node)
		if end-start <= 4 {
			return idx
		}
		ext := box.max.Sub(box.min)
		axis := 0
		if ext.Y > ext.X && ext.Y >= ext.Z {
			axis = 1
		} else if ext.Z > ext.X && ext.Z > ext.Y {
			axis = 2
		}
		sort.Slice(tr.items[start:end], func(i, j int) bool {
			a, b := boxes[tr.items[start+i]], boxes[tr.items[start+j]]
			ca := (a.min.X + a.max.X) * 0.5
			cb := (b.min.X + b.max.X) * 0.5
			if axis == 1 {
				ca = (a.min.Y + a.max.Y) * 0.5
				cb = (b.min.Y + b.max.Y) * 0.5
			} else if axis == 2 {
				ca = (a.min.Z + a.max.Z) * 0.5
				cb = (b.min.Z + b.max.Z) * 0.5
			}
			if ca == cb {
				return tr.items[start+i] < tr.items[start+j]
			}
			return ca < cb
		})
		mid := (start + end) / 2
		left := build(start, mid)
		right := build(mid, end)
		tr.nodes[idx].left = left
		tr.nodes[idx].right = right
		return idx
	}
	if n > 0 {
		build(0, n)
	}
	return tr
}

func (t bvh) query(box aabb, fn func(face int)) {
	if len(t.nodes) == 0 {
		return
	}
	stack := []int{0}
	for len(stack) > 0 {
		ni := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := t.nodes[ni]
		if !overlaps(n.box, box, 0) {
			continue
		}
		if n.left < 0 {
			for _, fi := range t.items[n.start:n.end] {
				fn(fi)
			}
			continue
		}
		stack = append(stack, n.left, n.right)
	}
}

func sharedVertex(a, b Face) bool {
	ai, bi := a.Indices(), b.Indices()
	for _, x := range ai {
		for _, y := range bi {
			if x == y {
				return true
			}
		}
	}
	return false
}

// CountSelfIntersections reports non-adjacent triangle pairs that intersect.
// Shared vertices/edges are excluded because they are valid mesh adjacency.
func CountSelfIntersections(m Mesh) int {
	if len(m.Faces) < 2 {
		return 0
	}
	tr := buildBVH(m)
	boxes := make([]aabb, len(m.Faces))
	for i := range boxes {
		boxes[i] = faceAABB(m, i)
	}
	var count int
	for i := range m.Faces {
		box := expand(boxes[i], 1e-12)
		tr.query(box, func(j int) {
			if j <= i || sharedVertex(m.Faces[i], m.Faces[j]) {
				return
			}
			if trianglesIntersect(m, i, j) {
				count++
			}
		})
	}
	return count
}

func trianglesIntersect(m Mesh, i, j int) bool {
	fi, fj := m.Faces[i], m.Faces[j]
	a := [3]Vec3{m.Vertices[fi.A], m.Vertices[fi.B], m.Vertices[fi.C]}
	b := [3]Vec3{m.Vertices[fj.A], m.Vertices[fj.B], m.Vertices[fj.C]}
	n1 := b[1].Sub(b[0]).Cross(b[2].Sub(b[0]))
	n2 := a[1].Sub(a[0]).Cross(a[2].Sub(a[0]))
	if n1.LenSq() == 0 || n2.LenSq() == 0 {
		return false
	}
	d1 := [3]float64{}
	d2 := [3]float64{}
	for k := 0; k < 3; k++ {
		d1[k] = n1.Dot(a[k].Sub(b[0]))
		d2[k] = n2.Dot(b[k].Sub(a[0]))
	}
	scale := math.Max(n1.Len(), n2.Len())
	eps := 1e-10 * scale * scale
	if allSameSide(d1, eps) || allSameSide(d2, eps) {
		return false
	}
	dir := n1.Cross(n2)
	if dir.LenSq() < eps*eps {
		return false
	}
	dir = dir.Normalize()
	i1 := trianglePlaneInterval(a, d1, dir, eps)
	i2 := trianglePlaneInterval(b, d2, dir, eps)
	return i1[0] <= i2[1]+eps && i2[0] <= i1[1]+eps
}

func allSameSide(d [3]float64, eps float64) bool {
	pos, neg := false, false
	for _, v := range d {
		if v > eps {
			pos = true
		} else if v < -eps {
			neg = true
		}
	}
	return pos != neg && (pos || neg)
}

func trianglePlaneInterval(p [3]Vec3, d [3]float64, dir Vec3, eps float64) [2]float64 {
	values := make([]float64, 0, 4)
	for i := 0; i < 3; i++ {
		j := (i + 1) % 3
		if math.Abs(d[i]) <= eps {
			values = append(values, p[i].Dot(dir))
		}
		if (d[i] > eps && d[j] < -eps) || (d[i] < -eps && d[j] > eps) {
			t := d[i] / (d[i] - d[j])
			q := p[i].Add(p[j].Sub(p[i]).Mul(t))
			values = append(values, q.Dot(dir))
		}
	}
	if len(values) == 0 {
		return [2]float64{0, 0}
	}
	lo, hi := values[0], values[0]
	for _, v := range values[1:] {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	return [2]float64{lo, hi}
}
