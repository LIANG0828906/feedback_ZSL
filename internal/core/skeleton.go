package core

import (
	"math"
	"sort"
)

type SkeletonIndex struct {
	Skeleton Skeleton
	ByName   map[string]int
	Children map[string][]string
	Depth    map[string]int
	Order    []int
}

func NewSkeletonIndex(sk Skeleton) (*SkeletonIndex, *Error) {
	idx := &SkeletonIndex{
		Skeleton: sk,
		ByName:   make(map[string]int, len(sk.Joints)),
		Children: make(map[string][]string),
		Depth:    make(map[string]int),
	}
	for i, j := range sk.Joints {
		if j.Name == "" {
			return nil, NewError(ECodeSchema, "skeleton contains a joint with empty name")
		}
		if _, exists := idx.ByName[j.Name]; exists {
			return nil, NewError(ECodeSchema, "duplicate joint name: "+j.Name)
		}
		idx.ByName[j.Name] = i
	}
	for _, j := range sk.Joints {
		if j.Parent != "" {
			if _, ok := idx.ByName[j.Parent]; !ok {
				return nil, &Error{Code: ECodeSchema, Joint: j.Name, Message: "parent reference is missing: " + j.Parent}
			}
			idx.Children[j.Parent] = append(idx.Children[j.Parent], j.Name)
		}
	}
	if err := idx.checkCycle(); err != nil {
		return nil, err
	}
	idx.buildDepthAndOrder()
	return idx, nil
}

func (x *SkeletonIndex) checkCycle() *Error {
	state := make(map[string]int, len(x.ByName)) // 0 unvisited, 1 visiting, 2 done
	var visit func(name string, path []string) *Error
	visit = func(name string, path []string) *Error {
		switch state[name] {
		case 1:
			cycle := append(append([]string{}, path...), name)
			return &Error{Code: ECodeCycle, Joint: name, Message: "skeleton parent references form a cycle", Details: cycle}
		case 2:
			return nil
		}
		state[name] = 1
		joint := x.Skeleton.Joints[x.ByName[name]]
		if joint.Parent != "" {
			if err := visit(joint.Parent, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	for _, j := range x.Skeleton.Joints {
		if err := visit(j.Name, nil); err != nil {
			return err
		}
	}
	return nil
}

func (x *SkeletonIndex) buildDepthAndOrder() {
	var depth func(name string) int
	depth = func(name string) int {
		if d, ok := x.Depth[name]; ok {
			return d
		}
		j := x.Skeleton.Joints[x.ByName[name]]
		if j.Parent == "" {
			x.Depth[name] = 0
		} else {
			x.Depth[name] = depth(j.Parent) + 1
		}
		return x.Depth[name]
	}
	for _, j := range x.Skeleton.Joints {
		x.Order = append(x.Order, x.ByName[j.Name])
		depth(j.Name)
	}
	sort.SliceStable(x.Order, func(a, b int) bool {
		return x.Depth[x.Skeleton.Joints[x.Order[a]].Name] < x.Depth[x.Skeleton.Joints[x.Order[b]].Name]
	})
}

func (x *SkeletonIndex) BoneLength(jointName string) float64 {
	i, ok := x.ByName[jointName]
	if !ok {
		return 0
	}
	return x.Skeleton.Joints[i].Offset.Norm()
}

type Pose struct {
	Root     Vec3
	Local    []Quat
	WorldRot []Quat
	WorldPos []Vec3
}

func ForwardKinematics(sk Skeleton, root Vec3, local []Quat) (*Pose, *Error) {
	idx, err := NewSkeletonIndex(sk)
	if err != nil {
		return nil, err
	}
	if len(local) != len(sk.Joints) {
		e := &Error{Code: ECodeQuat, Frame: -1, Message: "rotation count does not match joint count"}
		if len(local) < len(sk.Joints) {
			e.Joint = sk.Joints[len(local)].Name
		} else {
			e.Joint = sk.Joints[len(sk.Joints)-1].Name
			e.Details = map[string]int{"rotations": len(local), "joints": len(sk.Joints)}
		}
		return nil, e
	}
	pose := &Pose{Root: root, Local: append([]Quat{}, local...), WorldRot: make([]Quat, len(sk.Joints)), WorldPos: make([]Vec3, len(sk.Joints))}
	for _, ii := range idx.Order {
		j := sk.Joints[ii]
		q := local[ii].Normalized()
		if j.Parent == "" {
			pose.WorldRot[ii] = q
			pose.WorldPos[ii] = root.Add(j.Offset)
			continue
		}
		pi := idx.ByName[j.Parent]
		pose.WorldRot[ii] = pose.WorldRot[pi].Mul(q).Normalized()
		pose.WorldPos[ii] = pose.WorldPos[pi].Add(pose.WorldRot[pi].Rotate(j.Offset))
	}
	return pose, nil
}

func TPose(sk Skeleton) (*Pose, *Error) {
	ids := make([]Quat, len(sk.Joints))
	for i := range ids {
		ids[i] = Quat{1, 0, 0, 0}
	}
	return ForwardKinematics(sk, Vec3{}, ids)
}

func finiteSkeleton(sk Skeleton) *Error {
	if sk.ID == "" {
		return NewError(ECodeSchema, "skeleton id is missing")
	}
	if len(sk.Joints) == 0 {
		return NewError(ECodeSchema, "skeleton has no joints")
	}
	for _, j := range sk.Joints {
		if !j.Offset.Finite() {
			return &Error{Code: ECodeSchema, Joint: j.Name, Message: "joint offset contains non-finite value"}
		}
		if j.Limit != nil {
			l := j.Limit
			if !FiniteFloat(l.MinRadians) || !FiniteFloat(l.MaxRadians) || !l.Axis.Finite() {
				return &Error{Code: ECodeSchema, Joint: j.Name, Message: "angle limit contains non-finite value"}
			}
			if l.MaxRadians < l.MinRadians || l.MaxRadians < 0 || l.MaxRadians > math.Pi*2 {
				return &Error{Code: ECodeSchema, Joint: j.Name, Message: "angle limit is invalid"}
			}
		}
		if j.Reach != nil {
			if !FiniteFloat(j.Reach.MaxDistance) || j.Reach.MaxDistance <= 0 {
				return &Error{Code: ECodeSchema, Joint: j.Name, Message: "reach limit is invalid"}
			}
		}
	}
	return nil
}
