from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Iterable

from .bvh import BVHData
from .constraints import (
    BoneLengthConstraint,
    Constraint,
    ConstraintSet,
    EffectorTargetConstraint,
    JointLimitConstraint,
    frames_to_ranges,
    ranges_to_frames,
    union_ranges,
)


@dataclass(frozen=True)
class ImpactPlan:
    frames: tuple[tuple[int, int], ...]
    changed_constraints: tuple[str, ...]
    affected_joints: tuple[str, ...]
    reasons: tuple[str, ...]
    conservative: bool
    details: tuple[dict[str, Any], ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "frames": [list(pair) for pair in self.frames],
            "changed_constraints": list(self.changed_constraints),
            "affected_joints": list(self.affected_joints),
            "reasons": list(self.reasons),
            "conservative": self.conservative,
            "details": list(self.details),
        }

    def frame_set(self) -> set[int]:
        return ranges_to_frames(self.frames)


def _children_map(bvh: BVHData) -> dict[int, tuple[int, ...]]:
    children: dict[int, list[int]] = {
        index: [] for index in range(len(bvh.joints))
    }
    for index, joint in enumerate(bvh.joints):
        if joint.parent_index >= 0:
            children[joint.parent_index].append(index)
    return {key: tuple(value) for key, value in children.items()}


def _ancestors(bvh: BVHData, index: int) -> set[int]:
    result: set[int] = set()
    cursor = bvh.joints[index].parent_index
    while cursor >= 0:
        result.add(cursor)
        cursor = bvh.joints[cursor].parent_index
    return result


def _subtree(children: dict[int, tuple[int, ...]], index: int) -> set[int]:
    result = {index}
    stack = [index]
    while stack:
        current = stack.pop()
        for child in children[current]:
            result.add(child)
            stack.append(child)
    return result


def _target_map(
    constraint: EffectorTargetConstraint,
) -> dict[int, tuple[float, float, float]]:
    return {frame: target for frame, target in constraint.targets}


def derive_impact(
    bvh: BVHData,
    previous: ConstraintSet | None,
    current: ConstraintSet,
) -> ImpactPlan:
    all_frames = set(range(bvh.frame_count))
    if previous is None:
        return ImpactPlan(
            frames=((0, bvh.frame_count - 1),) if bvh.frame_count else (),
            changed_constraints=tuple(
                sorted(constraint.id for constraint in current.constraints)
            ),
            affected_joints=tuple(
                sorted(joint.name for joint in bvh.joints)
            ),
            reasons=("initial_solve_all_frames",),
            conservative=True,
            details=(
                {
                    "reason": "initial_solve_all_frames",
                    "explanation": (
                        "No previous committed parameter state exists; every frame "
                        "must be considered."
                    ),
                },
            ),
        )

    if previous.solve != current.solve:
        changed = sorted(
            key
            for key in current.solve.to_dict()
            if getattr(previous.solve, key) != getattr(current.solve, key)
        )
        return ImpactPlan(
            frames=((0, bvh.frame_count - 1),) if bvh.frame_count else (),
            changed_constraints=tuple(
                sorted(
                    set(item.id for item in previous.constraints)
                    | set(item.id for item in current.constraints)
                )
            ),
            affected_joints=tuple(sorted(joint.name for joint in bvh.joints)),
            reasons=("global_solver_parameters_changed",),
            conservative=True,
            details=(
                {
                    "reason": "global_solver_parameters_changed",
                    "fields": changed,
                    "explanation": (
                        "Global iteration or step parameters influence every frame; "
                        "a full dependency closure is required."
                    ),
                },
            ),
        )

    old_by_id = {constraint.id: constraint for constraint in previous.constraints}
    new_by_id = {constraint.id: constraint for constraint in current.constraints}
    all_ids = sorted(set(old_by_id) | set(new_by_id))
    child_map = _children_map(bvh)
    affected_frames: set[int] = set()
    affected_joints: set[int] = set()
    changed_ids: list[str] = []
    reasons: set[str] = set()
    details: list[dict[str, Any]] = []
    conservative = False

    for constraint_id in all_ids:
        old_constraint = old_by_id.get(constraint_id)
        new_constraint = new_by_id.get(constraint_id)
        if old_constraint is None and new_constraint is None:
            continue
        if (
            old_constraint is not None
            and new_constraint is not None
            and old_constraint.to_dict() == new_constraint.to_dict()
        ):
            continue
        changed_ids.append(constraint_id)
        old_frames = (
            set(ranges_to_frames(old_constraint.frames))
            if old_constraint is not None
            else set()
        )
        new_frames = (
            set(ranges_to_frames(new_constraint.frames))
            if new_constraint is not None
            else set()
        )
        old_joints = (
            {old_constraint.joint}
            if old_constraint is not None
            else set()
        )
        new_joints = (
            {new_constraint.joint}
            if new_constraint is not None
            else set()
        )
        joint_names = old_joints | new_joints
        for joint_name in joint_names:
            joint_index = bvh.joint_by_name.get(joint_name)
            if joint_index is None:
                conservative = True
                affected_frames.update(all_frames)
                reasons.add("unresolved_joint_dependency")
                details.append(
                    {
                        "constraint_id": constraint_id,
                        "reason": "unresolved_joint_dependency",
                        "joint": joint_name,
                    }
                )
                continue
            affected_joints.update(_ancestors(bvh, joint_index))
            affected_joints.add(joint_index)
            if isinstance(
                old_constraint, (BoneLengthConstraint, JointLimitConstraint)
            ) or isinstance(
                new_constraint, (BoneLengthConstraint, JointLimitConstraint)
            ):
                affected_joints.update(_subtree(child_map, joint_index))

        if old_constraint is None:
            frames = new_frames
            reasons.add("constraint_added")
            detail_reason = "constraint_added"
        elif new_constraint is None:
            frames = old_frames
            reasons.add("constraint_removed")
            detail_reason = "constraint_removed"
        elif type(old_constraint) is not type(new_constraint):
            frames = old_frames | new_frames
            reasons.add("constraint_type_changed")
            detail_reason = "constraint_type_changed"
        else:
            frames = old_frames | new_frames
            detail_reason = "constraint_parameters_changed"
            reasons.add("constraint_parameters_changed")
            if (
                isinstance(old_constraint, EffectorTargetConstraint)
                and isinstance(new_constraint, EffectorTargetConstraint)
                and _target_map(old_constraint) != _target_map(new_constraint)
                and old_constraint.weight == new_constraint.weight
                and old_constraint.tolerance == new_constraint.tolerance
                and old_constraint.joint == new_constraint.joint
                and old_constraint.frames == new_constraint.frames
            ):
                old_targets = _target_map(old_constraint)
                new_targets = _target_map(new_constraint)
                frames = {
                    frame
                    for frame in set(old_targets) | set(new_targets)
                    if old_targets.get(frame) != new_targets.get(frame)
                }
                detail_reason = "effector_target_changed"
                reasons.add("effector_target_changed")
        affected_frames.update(frames)
        details.append(
            {
                "constraint_id": constraint_id,
                "reason": detail_reason,
                "frames": [list(pair) for pair in frames_to_ranges(frames)],
                "joints": sorted(joint_names),
            }
        )

    if changed_ids:
        # An effector target is affected when its joint lies on any changed
        # parent-child chain, but only for frames in the parameter change closure.
        affected_joint_names = {
            bvh.joints[index].name for index in affected_joints
        }
        for constraint in current.constraints:
            if not isinstance(constraint, EffectorTargetConstraint):
                continue
            if constraint.joint not in affected_joint_names:
                continue
            target_frames = {frame for frame, _ in constraint.targets}
            closure = target_frames & affected_frames
            if closure:
                affected_frames.update(closure)
                reasons.add("effector_target_frame_closure")
                details.append(
                    {
                        "constraint_id": constraint.id,
                        "reason": "effector_target_frame_closure",
                        "frames": [
                            list(pair) for pair in frames_to_ranges(closure)
                        ],
                        "joint": constraint.joint,
                    }
                )

    if not affected_frames and changed_ids:
        reasons.add("metadata_or_empty_frame_scope_only")
    if not changed_ids and previous.version != current.version:
        reasons.add("metadata_only_version_change")

    ranges = frames_to_ranges(affected_frames)
    return ImpactPlan(
        frames=ranges,
        changed_constraints=tuple(changed_ids),
        affected_joints=tuple(
            sorted(bvh.joints[index].name for index in affected_joints)
        ),
        reasons=tuple(sorted(reasons)),
        conservative=conservative,
        details=tuple(details),
    )
