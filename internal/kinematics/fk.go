package kinematics

import (
	"fmt"
	"sort"

	"retargetlab/internal/geom"
	"retargetlab/internal/model"
)

type WorldPose struct {
	Positions []geom.Vec3
	Rotations []geom.Quat
}

func TopologicalOrder(s model.Skeleton) ([]int, error) {
	children := make([][]int, len(s.Joints))
	roots := make([]int, 0, 1)
	for i, j := range s.Joints {
		if j.Parent < 0 {
			roots = append(roots, i)
			continue
		}
		if j.Parent >= len(s.Joints) || j.Parent == i {
			return nil, fmt.Errorf("invalid parent for joint %q", j.Name)
		}
		children[j.Parent] = append(children[j.Parent], i)
	}
	if len(roots) != 1 {
		return nil, fmt.Errorf("expected exactly one root, got %d", len(roots))
	}
	order := make([]int, 0, len(s.Joints))
	stack := append([]int(nil), roots...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		order = append(order, cur)
		for i := len(children[cur]) - 1; i >= 0; i-- {
			stack = append(stack, children[cur][i])
		}
	}
	if len(order) != len(s.Joints) {
		return nil, fmt.Errorf("skeleton contains unreachable joints")
	}
	return order, nil
}

func Forward(s model.Skeleton, rootTranslation geom.Vec3, localRotations []geom.Quat) (WorldPose, error) {
	if len(localRotations) != len(s.Joints) {
		return WorldPose{}, fmt.Errorf("rotation count %d does not match joint count %d", len(localRotations), len(s.Joints))
	}
	order, err := TopologicalOrder(s)
	if err != nil {
		return WorldPose{}, err
	}
	wp := WorldPose{
		Positions: make([]geom.Vec3, len(s.Joints)),
		Rotations: make([]geom.Quat, len(s.Joints)),
	}
	for _, idx := range order {
		j := s.Joints[idx]
		local := localRotations[idx].Normalize()
		if j.Parent < 0 {
			wp.Positions[idx] = rootTranslation.Add(j.LocalOffset)
			wp.Rotations[idx] = local
			continue
		}
		wp.Positions[idx] = wp.Positions[j.Parent].Add(wp.Rotations[j.Parent].Rotate(j.LocalOffset))
		wp.Rotations[idx] = wp.Rotations[j.Parent].Mul(local).Normalize()
	}
	return wp, nil
}

// ContactCorrectedRoot returns the root translation that places the lowest
// contacting foot at y=0, plus the contact foot names and applied shift.
func ContactCorrectedRoot(s model.Skeleton, root geom.Vec3, local []geom.Quat, contacts []model.ContactInterval, frame int) (geom.Vec3, geom.Vec3, []string, error) {
	wp, err := Forward(s, root, local)
	if err != nil {
		return root, geom.Vec3{}, nil, err
	}
	minY := 0.0
	found := false
	feet := make([]string, 0, 2)
	for _, c := range contacts {
		if !c.Contains(frame) {
			continue
		}
		idx := s.JointIndex(c.Foot)
		if idx < 0 {
			return root, geom.Vec3{}, nil, fmt.Errorf("contact foot %q is not present in target skeleton", c.Foot)
		}
		y := wp.Positions[idx].Y
		if !found || y < minY {
			minY = y
			found = true
		}
		feet = append(feet, c.Foot)
	}
	if !found {
		return root, geom.Vec3{}, nil, nil
	}
	sort.Strings(feet)
	shift := geom.Vec3{Y: -minY}
	return root.Add(shift), shift, feet, nil
}

func BoneLengths(s model.Skeleton) map[string]float64 {
	out := make(map[string]float64, len(s.Joints))
	for i := range s.Joints {
		p := s.Joints[i].Parent
		if p < 0 {
			out[s.Joints[i].Name] = 0
			continue
		}
		out[s.Joints[i].Name] = s.Joints[i].LocalOffset.Length()
	}
	return out
}
