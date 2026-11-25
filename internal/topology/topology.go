package topology

import (
	"skeletonretarget/internal/model"
)

func SortSkeleton(joints []model.Joint) ([]model.Joint, *model.Error) {
	if len(joints) == 0 {
		return nil, &model.Error{Code: model.CodeTopo, Frame: -1, Message: "skeleton has no joints"}
	}
	byID := make(map[string]int, len(joints))
	for i, joint := range joints {
		if joint.ID == "" {
			return nil, &model.Error{Code: model.CodeTopo, Joint: joint.ID, Frame: -1, Message: "empty joint id"}
		}
		if _, exists := byID[joint.ID]; exists {
			return nil, &model.Error{Code: model.CodeTopo, Joint: joint.ID, Frame: -1, Message: "duplicate joint id"}
		}
		byID[joint.ID] = i
	}
	for _, joint := range joints {
		if joint.Parent != "" {
			if _, ok := byID[joint.Parent]; !ok {
				return nil, &model.Error{Code: model.CodeTopo, Joint: joint.ID, Frame: -1, Message: "parent " + joint.Parent + " does not exist"}
			}
		}
	}

	state := make([]uint8, len(joints))
	var order []model.Joint
	var visit func(index int) *model.Error
	visit = func(index int) *model.Error {
		switch state[index] {
		case 1:
			return &model.Error{Code: model.CodeTopo, Joint: joints[index].ID, Frame: -1, Message: "skeleton cycle"}
		case 2:
			return nil
		}
		state[index] = 1
		joint := joints[index]
		if joint.Parent != "" {
			if err := visit(byID[joint.Parent]); err != nil {
				return err
			}
		}
		state[index] = 2
		order = append(order, joint)
		return nil
	}
	for i := range joints {
		if state[i] == 0 {
			if err := visit(i); err != nil {
				return nil, err
			}
		}
	}
	return order, nil
}

func SortTarget(joints []model.TargetJoint) ([]model.TargetJoint, *model.Error) {
	if len(joints) == 0 {
		return nil, &model.Error{Code: model.CodeTopo, Frame: -1, Message: "target skeleton has no joints"}
	}
	byID := make(map[string]int, len(joints))
	for i, joint := range joints {
		if joint.ID == "" {
			return nil, &model.Error{Code: model.CodeTopo, Joint: joint.ID, Frame: -1, Message: "empty target joint id"}
		}
		if _, exists := byID[joint.ID]; exists {
			return nil, &model.Error{Code: model.CodeTopo, Joint: joint.ID, Frame: -1, Message: "duplicate target joint id"}
		}
		byID[joint.ID] = i
	}
	for _, joint := range joints {
		if joint.Parent != "" {
			if _, ok := byID[joint.Parent]; !ok {
				return nil, &model.Error{Code: model.CodeTopo, Joint: joint.ID, Frame: -1, Message: "target parent " + joint.Parent + " does not exist"}
			}
		}
	}
	state := make([]uint8, len(joints))
	var order []model.TargetJoint
	var visit func(index int) *model.Error
	visit = func(index int) *model.Error {
		switch state[index] {
		case 1:
			return &model.Error{Code: model.CodeTopo, Joint: joints[index].ID, Frame: -1, Message: "target skeleton cycle"}
		case 2:
			return nil
		}
		state[index] = 1
		joint := joints[index]
		if joint.Parent != "" {
			if err := visit(byID[joint.Parent]); err != nil {
				return err
			}
		}
		state[index] = 2
		order = append(order, joint)
		return nil
	}
	for i := range joints {
		if state[i] == 0 {
			if err := visit(i); err != nil {
				return nil, err
			}
		}
	}
	return order, nil
}
