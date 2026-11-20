"""Deterministic local constraint solver and adaptive execution strategies."""

from __future__ import annotations

import hashlib
import math
import threading
from collections.abc import Callable, Iterable, Sequence
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any

import numpy as np

from .bvh import BvhDocument, Pose
from .constraints import ConstraintSet, ConstraintSpec
from .errors import E_CONFIG_INVALID, BusinessError
from .math3d import (
    canonical_json_bytes,
    euler_matrix,
    hash_array,
    hash_pose,
    hash_text,
    matrix_to_euler,
    matrix_to_rotation_vector,
    orthonormalize,
    rotation_vector_to_matrix,
)

STRATEGIES = {"auto", "batch", "window"}


def _config_fail(message: str, **details: object) -> None:
    raise BusinessError(E_CONFIG_INVALID, message, details)


@dataclass(frozen=True)
class SolverConfig:
    max_iterations: int = 24
    damping: float = 2.0e-3
    step_limit: float = 0.65
    batch_frame_limit: int = 600
    batch_complexity_limit: int = 8_000_000
    window_size: int = 96
    window_overlap: int = 12
    chunk_size: int = 240
    memory_budget_bytes: int = 512 * 1024 * 1024
    workers: int = 4
    strategy: str = "auto"
    line_search_steps: int = 5
    cache_max_entries: int = 200_000

    def validate(self) -> None:
        if self.max_iterations < 1:
            _config_fail("max_iterations must be at least 1")
        if self.damping <= 0.0 or not math.isfinite(self.damping):
            _config_fail("damping must be positive")
        if self.step_limit <= 0.0 or not math.isfinite(self.step_limit):
            _config_fail("step_limit must be positive")
        if self.batch_frame_limit < 1:
            _config_fail("batch_frame_limit must be at least 1")
        if self.batch_complexity_limit < 1:
            _config_fail("batch_complexity_limit must be at least 1")
        if self.window_size < 2:
            _config_fail("window_size must be at least 2")
        if self.window_overlap < 0 or self.window_overlap >= self.window_size:
            _config_fail("window_overlap must satisfy 0 <= overlap < window_size")
        if self.chunk_size < 1:
            _config_fail("chunk_size must be at least 1")
        if self.memory_budget_bytes < 1:
            _config_fail("memory_budget_bytes must be positive")
        if self.workers < 1:
            _config_fail("workers must be at least 1")
        if self.strategy not in STRATEGIES:
            _config_fail("strategy must be auto, batch or window")
        if self.line_search_steps < 1:
            _config_fail("line_search_steps must be at least 1")
        if self.cache_max_entries < 1:
            _config_fail("cache_max_entries must be at least 1")

    def to_dict(self) -> dict[str, Any]:
        return {
            "max_iterations": self.max_iterations,
            "damping": self.damping,
            "step_limit": self.step_limit,
            "batch_frame_limit": self.batch_frame_limit,
            "batch_complexity_limit": self.batch_complexity_limit,
            "window_size": self.window_size,
            "window_overlap": self.window_overlap,
            "chunk_size": self.chunk_size,
            "memory_budget_bytes": self.memory_budget_bytes,
            "workers": self.workers,
            "strategy": self.strategy,
            "line_search_steps": self.line_search_steps,
            "cache_max_entries": self.cache_max_entries,
        }

    def solve_signature(self) -> str:
        payload = {
            "max_iterations": self.max_iterations,
            "damping": self.damping,
            "step_limit": self.step_limit,
            "line_search_steps": self.line_search_steps,
        }
        return hash_text(canonical_json_bytes(payload).decode("utf-8"))

    def execution_signature(self) -> str:
        return hash_text(canonical_json_bytes(self.to_dict()).decode("utf-8"))


@dataclass(frozen=True)
class ConstraintResidual:
    constraint_id: str
    kind: str
    joint: str
    frame: int
    residual: float
    tolerance: float
    weight: float
    satisfied: bool
    applicable: bool
    message: str = ""

    @property
    def weighted_squared(self) -> float:
        if not self.applicable:
            return 0.0
        return self.weight * self.residual * self.residual

    def to_dict(self) -> dict[str, Any]:
        return {
            "constraint_id": self.constraint_id,
            "kind": self.kind,
            "joint": self.joint,
            "frame": int(self.frame),
            "residual": float(self.residual),
            "tolerance": float(self.tolerance),
            "weight": float(self.weight),
            "satisfied": bool(self.satisfied),
            "applicable": bool(self.applicable),
            "message": self.message,
        }


@dataclass
class FrameResult:
    frame: int
    pose: Pose
    residuals: tuple[ConstraintResidual, ...]
    total_residual: float
    satisfied: bool
    conflicts: tuple[str, ...]
    pose_hash: str
    cache_hit: bool
    strategy: str
    iterations: int
    diagnostic: dict[str, Any] | None = None
    restored: bool = False

    @property
    def joint_positions(self) -> np.ndarray:
        return self.pose.joint_positions

    def log_record(self) -> dict[str, Any]:
        return {
            "frame": int(self.frame),
            "strategy": self.strategy,
            "total_residual": float(self.total_residual),
            "conflict": bool(self.conflicts),
            "conflict_constraints": list(self.conflicts),
            "cache_hit": bool(self.cache_hit),
            "restored": bool(self.restored),
            "pose_hash": self.pose_hash,
            "residuals": [item.to_dict() for item in self.residuals],
        }


@dataclass
class FramePoseEntry:
    pose: Pose
    pose_hash: str
    iterations: int


class SolveCache:
    """Thread-safe deterministic frame pose cache.

    Tolerances are deliberately excluded from the key: changing a tolerance
    only requires re-evaluating residuals, not re-running the local solver.
    Constraint weights and targets are included, so editing one frame-specific
    target preserves hits for every unaffected frame.
    """

    def __init__(self, max_entries: int = 200_000):
        self.max_entries = max_entries
        self._entries: dict[str, FramePoseEntry] = {}
        self._order: list[str] = []
        self._lock = threading.RLock()
        self.hits = 0
        self.misses = 0

    def get(self, key: str) -> FramePoseEntry | None:
        with self._lock:
            value = self._entries.get(key)
            if value is None:
                self.misses += 1
                return None
            self.hits += 1
            return value

    def put(self, key: str, value: FramePoseEntry) -> None:
        with self._lock:
            if key not in self._entries:
                self._order.append(key)
            self._entries[key] = value
            while len(self._order) > self.max_entries:
                old = self._order.pop(0)
                self._entries.pop(old, None)

    def invalidate_frames(self, document: BvhDocument, frames: Iterable[int]) -> int:
        prefixes = {f"{document.skeleton_fingerprint()}:{int(frame)}:" for frame in frames}
        with self._lock:
            doomed = [
                key
                for key in self._entries
                if any(key.startswith(prefix) for prefix in prefixes)
            ]
            for key in doomed:
                self._entries.pop(key, None)
            if doomed:
                doomed_set = set(doomed)
                self._order = [key for key in self._order if key not in doomed_set]
            return len(doomed)

    def clear(self) -> None:
        with self._lock:
            self._entries.clear()
            self._order.clear()
            self.hits = 0
            self.misses = 0

    def stats(self) -> dict[str, int]:
        with self._lock:
            return {
                "hits": self.hits,
                "misses": self.misses,
                "entries": len(self._entries),
            }


def select_strategy(
    document: BvhDocument,
    constraints: ConstraintSet,
    config: SolverConfig,
) -> str:
    config.validate()
    if config.strategy != "auto":
        return config.strategy
    rotating = sum(
        1 for joint in document.joints if joint.rotation_channels and not joint.is_end_site
    )
    complexity = document.frame_count * len(constraints.constraints) * max(rotating, 1)
    if (
        document.frame_count <= config.batch_frame_limit
        and complexity <= config.batch_complexity_limit
    ):
        return "batch"
    return "window"


def _limit_bounds_for_joint(
    constraints: ConstraintSet, joint_name: str
) -> dict[str, tuple[float, float]]:
    out: dict[str, tuple[float, float]] = {}
    for item in constraints.constraints:
        if item.kind != "joint_limit" or item.joint != joint_name or item.axis is None:
            continue
        if not item.active:
            continue
        lo = float(item.minimum)
        hi = float(item.maximum)
        if item.axis in out:
            old_lo, old_hi = out[item.axis]
            lo = max(lo, old_lo)
            hi = min(hi, old_hi)
        out[item.axis] = (lo, hi)
    return out


def project_joint_limits(
    document: BvhDocument,
    constraints: ConstraintSet,
    base_rotations: np.ndarray,
    deltas: np.ndarray,
) -> np.ndarray:
    out = np.array(deltas, dtype=np.float64, copy=True)
    for index, joint in enumerate(document.joints):
        bounds = _limit_bounds_for_joint(constraints, joint.name)
        if not bounds:
            continue
        order = document.rotation_order(index)
        current = base_rotations[index] @ rotation_vector_to_matrix(out[index])
        angles = matrix_to_euler(order, current)
        changed = False
        for axis, (lo, hi) in bounds.items():
            slot = order.index(axis)
            value = float(angles[slot])
            clamped = min(max(value, lo), hi)
            if abs(clamped - value) > 1.0e-15:
                angles[slot] = clamped
                changed = True
        if changed:
            clamped_rotation = euler_matrix(order, angles)
            relative = base_rotations[index].T @ clamped_rotation
            out[index] = matrix_to_rotation_vector(relative)
    return out


def evaluate_residuals(
    document: BvhDocument,
    constraints: ConstraintSet,
    frame_index: int,
    pose: Pose,
) -> tuple[tuple[ConstraintResidual, ...], float, bool, tuple[str, ...], dict[str, Any] | None]:
    residuals: list[ConstraintResidual] = []
    for item in constraints.constraints:
        applicable = item.applicable(frame_index)
        residual = 0.0
        message = ""
        if not applicable:
            message = "target marker missing; constraint skipped"
        elif item.kind == "bone_length":
            joint_index = document.joint_names.index(item.joint)
            parent = document.joints[joint_index].parent
            if parent is None:
                actual = 0.0
            else:
                actual = float(
                    np.linalg.norm(
                        pose.joint_positions[joint_index] - pose.joint_positions[parent]
                    )
                )
            residual = abs(actual - float(item.target_length))
        elif item.kind == "joint_limit":
            joint_index = document.joint_names.index(item.joint)
            order = document.rotation_order(joint_index)
            angles = matrix_to_euler(order, pose.local_rotations[joint_index])
            angle = float(angles[order.index(item.axis)])
            residual = max(float(item.minimum) - angle, angle - float(item.maximum), 0.0)
        else:
            target = item.target_for_frame(frame_index)
            if target is not None:
                joint_index = document.joint_names.index(item.joint)
                residual = float(np.linalg.norm(pose.joint_positions[joint_index] - target))
        enabled = item.active
        satisfied = (not applicable) or (not enabled) or residual <= item.tolerance + 1.0e-12
        residuals.append(
            ConstraintResidual(
                constraint_id=item.id,
                kind=item.kind,
                joint=item.joint,
                frame=int(frame_index),
                residual=float(residual),
                tolerance=float(item.tolerance),
                weight=float(item.weight),
                satisfied=bool(satisfied),
                applicable=bool(applicable),
                message=message,
            )
        )
    weighted = [item.weighted_squared for item in residuals]
    total = math.sqrt(float(sum(weighted)))
    conflicts = tuple(
        item.constraint_id
        for item in residuals
        if item.applicable and item.weight > 0.0 and not item.satisfied
    )
    diagnostic: dict[str, Any] | None = None
    if conflicts:
        worst = max(
            (item for item in residuals if item.constraint_id in conflicts),
            key=lambda item: (item.residual - item.tolerance, item.constraint_id),
        )
        details = []
        for item in residuals:
            if item.constraint_id in conflicts:
                details.append(
                    {
                        "constraint_id": item.constraint_id,
                        "kind": item.kind,
                        "joint": item.joint,
                        "residual": item.residual,
                        "tolerance": item.tolerance,
                        "excess": max(0.0, item.residual - item.tolerance),
                    }
                )
        diagnostic = {
            "frame": int(frame_index),
            "conflict_constraints": list(conflicts),
            "joints": sorted({item.joint for item in residuals if item.constraint_id in conflicts}),
            "worst_constraint_id": worst.constraint_id,
            "worst_joint": worst.joint,
            "worst_residual": worst.residual,
            "worst_tolerance": worst.tolerance,
            "worst_excess": max(0.0, worst.residual - worst.tolerance),
            "evidence": (
                f"frame {frame_index}: {worst.constraint_id} on {worst.joint} "
                f"residual {worst.residual:.9g} exceeds tolerance {worst.tolerance:.9g}"
            ),
            "details": details,
        }
    return tuple(residuals), total, not conflicts, conflicts, diagnostic


def _active_effectors(
    constraints: ConstraintSet, frame_index: int
) -> list[tuple[ConstraintSpec, np.ndarray]]:
    out: list[tuple[ConstraintSpec, np.ndarray]] = []
    for item in constraints.constraints:
        if item.kind != "effector" or not item.active:
            continue
        target = item.target_for_frame(frame_index)
        if target is not None:
            out.append((item, target))
    return out


def _solver_variables(
    document: BvhDocument,
    effectors: Sequence[tuple[ConstraintSpec, np.ndarray]],
) -> list[tuple[int, int]]:
    joints: set[int] = set()
    for item, _ in effectors:
        joint_index = document.joint_names.index(item.joint)
        for ancestor in document.ancestor_chain(joint_index):
            joint = document.joints[ancestor]
            if joint.rotation_channels and not joint.is_end_site:
                joints.add(ancestor)
    return [(joint, axis) for joint in sorted(joints) for axis in range(3)]


def _objective(
    document: BvhDocument,
    constraints: ConstraintSet,
    frame_index: int,
    pose: Pose,
) -> float:
    total = 0.0
    for item in constraints.constraints:
        if not item.active or not item.applicable(frame_index):
            continue
        if item.kind == "effector":
            target = item.target_for_frame(frame_index)
            joint_index = document.joint_names.index(item.joint)
            value = float(np.linalg.norm(pose.joint_positions[joint_index] - target))
        elif item.kind == "joint_limit":
            joint_index = document.joint_names.index(item.joint)
            order = document.rotation_order(joint_index)
            angles = matrix_to_euler(order, pose.local_rotations[joint_index])
            angle = float(angles[order.index(item.axis)])
            value = max(float(item.minimum) - angle, angle - float(item.maximum), 0.0)
        else:
            joint_index = document.joint_names.index(item.joint)
            parent = document.joints[joint_index].parent
            actual = float(
                np.linalg.norm(
                    pose.joint_positions[joint_index] - pose.joint_positions[parent]
                )
            )
            value = abs(actual - float(item.target_length))
        total += item.weight * value * value
    return total


def _solve_local(
    document: BvhDocument,
    constraints: ConstraintSet,
    frame_index: int,
    config: SolverConfig,
) -> tuple[Pose, int]:
    base_rotations, translations = document.base_rotations_and_translations(frame_index)
    joint_count = document.joint_count
    deltas = np.zeros((joint_count, 3), dtype=np.float64)
    deltas = project_joint_limits(document, constraints, base_rotations, deltas)
    best_pose = document.evaluate_with_state(base_rotations, translations, deltas)
    best_cost = _objective(document, constraints, frame_index, best_pose)
    best_deltas = deltas.copy()
    effectors = _active_effectors(constraints, frame_index)
    if not effectors:
        return best_pose, 0
    variables = _solver_variables(document, effectors)
    if not variables:
        return best_pose, 0
    variable_joints = np.array([item[0] for item in variables], dtype=np.int64)
    descendant_masks: list[np.ndarray] = []
    for item, _ in effectors:
        effector_joint = document.joint_names.index(item.joint)
        descendant_masks.append(
            np.array(
                [
                    document.is_descendant(effector_joint, joint_index)
                    for joint_index in variable_joints
                ],
                dtype=bool,
            )
        )
    damping = float(config.damping)
    iterations = 0
    stalled = 0

    for iteration in range(config.max_iterations):
        pose = document.evaluate_with_state(base_rotations, translations, deltas)
        _residuals, _, satisfied, _, _ = evaluate_residuals(
            document, constraints, frame_index, pose
        )
        cost = _objective(document, constraints, frame_index, pose)
        if cost < best_cost:
            best_cost = cost
            best_pose = pose
            best_deltas = deltas.copy()
        if satisfied:
            iterations = iteration
            return pose, iterations

        active = [
            (item, target)
            for item, target in effectors
            if target is not None
        ]
        if not active:
            break
        rows: list[np.ndarray] = []
        rhs: list[float] = []
        world_rotations = pose.world[variable_joints, :3, :3]
        # Shape (variables, axes, components).
        world_axes = world_rotations.transpose(0, 2, 1)
        pivots = pose.joint_positions[variable_joints]
        for active_index, (item, target) in enumerate(active):
            joint_index = document.joint_names.index(item.joint)
            position = pose.joint_positions[joint_index]
            target_error = target - position
            delta = position[None, :] - pivots
            derivatives = np.cross(world_axes, delta[:, None, :])
            mask = descendant_masks[active_index]
            derivatives[~mask] = 0.0
            # (variable, axis, residual component) -> rows=components,
            # columns=(variable, axis), matching the step vector layout.
            row = -derivatives.transpose(2, 0, 1).reshape(3, -1)
            scale = math.sqrt(max(item.weight, 1.0e-12))
            rows.append(row * scale)
            rhs.extend((target_error * scale).tolist())
        if not rows:
            break
        jacobian = np.vstack(rows)
        residual_vector = np.asarray(rhs, dtype=np.float64)
        hessian = jacobian.T @ jacobian
        # The rows are d(residual)/d(rotation-vector).  The damped
        # Gauss-Newton step solves J^T J delta = -J^T residual.
        gradient = -(jacobian.T @ residual_vector)
        hessian.flat[:: hessian.shape[0] + 1] += damping
        try:
            step = np.linalg.solve(hessian, gradient)
        except np.linalg.LinAlgError:
            step = np.linalg.lstsq(hessian, gradient, rcond=None)[0]
        norm = float(np.linalg.norm(step))
        if norm > config.step_limit:
            step *= config.step_limit / norm

        accepted = False
        best_local_cost = best_cost
        best_local_deltas: np.ndarray | None = None
        best_local_pose: Pose | None = None
        for line in range(config.line_search_steps):
            alpha = 1.0 / (2.0**line)
            candidate = np.array(deltas, dtype=np.float64, copy=True)
            for var_index, (joint_index_k, axis) in enumerate(variables):
                candidate[joint_index_k, axis] += alpha * step[var_index * 3 + axis]
            candidate = project_joint_limits(
                document, constraints, base_rotations, candidate
            )
            candidate_pose = document.evaluate_with_state(
                base_rotations, translations, candidate
            )
            candidate_cost = _objective(
                document, constraints, frame_index, candidate_pose
            )
            if candidate_cost < best_local_cost - 1.0e-15:
                best_local_cost = candidate_cost
                best_local_deltas = candidate
                best_local_pose = candidate_pose
                accepted = True
                break
        iterations = iteration + 1
        if not accepted:
            stalled += 1
            damping = min(damping * 10.0, 1.0e6)
            if stalled >= 2:
                break
            continue
        stalled = 0
        damping = max(damping * 0.4, 1.0e-7)
        deltas = best_local_deltas  # type: ignore[assignment]
        if best_local_cost < best_cost:
            best_cost = best_local_cost
            best_pose = best_local_pose  # type: ignore[assignment]
            best_deltas = deltas.copy()

    final_pose = document.evaluate_with_state(base_rotations, translations, best_deltas)
    final_cost = _objective(document, constraints, frame_index, final_pose)
    if final_cost <= best_cost:
        return final_pose, iterations
    return best_pose, iterations


def solve_frame(
    document: BvhDocument,
    constraints: ConstraintSet,
    frame_index: int,
    config: SolverConfig,
    strategy: str,
    cache: SolveCache | None = None,
) -> FrameResult:
    config.validate()
    if frame_index < 0 or frame_index >= document.frame_count:
        _config_fail(
            "frame index is outside the motion",
            frame=frame_index,
            frame_count=document.frame_count,
        )
    cache_hit = False
    iterations = 0
    cache_key = ""
    if cache is not None:
        payload = {
            "skeleton": document.skeleton_fingerprint(),
            "frame": int(frame_index),
            "channels": hash_array(document.channel_vector(frame_index)),
            "constraints": constraints.solver_signature(document, frame_index),
            "solver": config.solve_signature(),
        }
        cache_key = f"{document.skeleton_fingerprint()}:{int(frame_index)}:" + hash_text(
            canonical_json_bytes(payload).decode("utf-8")
        )
        cached = cache.get(cache_key)
        if cached is not None:
            pose = cached.pose
            pose_hash = cached.pose_hash
            iterations = cached.iterations
            cache_hit = True
    if not cache_hit:
        pose, iterations = _solve_local(document, constraints, frame_index, config)
        pose_hash = hash_pose(document.joint_names, pose.world)
        if cache is not None:
            cache.put(
                cache_key,
                FramePoseEntry(pose=pose, pose_hash=pose_hash, iterations=iterations),
            )
    residuals, total, satisfied, conflicts, diagnostic = evaluate_residuals(
        document, constraints, frame_index, pose
    )
    return FrameResult(
        frame=int(frame_index),
        pose=pose,
        residuals=residuals,
        total_residual=float(total),
        satisfied=bool(satisfied),
        conflicts=conflicts,
        pose_hash=pose_hash,
        cache_hit=cache_hit,
        strategy=strategy,
        iterations=int(iterations),
        diagnostic=diagnostic,
    )


def _fuse_pose(
    document: BvhDocument,
    frame_index: int,
    first: Pose,
    second: Pose,
) -> Pose:
    if np.array_equal(first.world, second.world):
        return first
    world = np.empty_like(first.world)
    for index, joint in enumerate(document.joints):
        world[index, :3, 3] = 0.5 * (
            first.world[index, :3, 3] + second.world[index, :3, 3]
        )
        rotation = orthonormalize(
            0.5 * (first.world[index, :3, :3] + second.world[index, :3, :3])
        )
        world[index, :3, :3] = rotation
        world[index, 3, :] = (0.0, 0.0, 0.0, 1.0)
    local = np.empty_like(first.local_rotations)
    for index, joint in enumerate(document.joints):
        if joint.parent is None:
            parent_rotation = np.eye(3)
        else:
            parent_rotation = world[joint.parent, :3, :3]
        local[index] = parent_rotation.T @ world[index, :3, :3]
    positions = world[:, :3, 3].copy()
    return Pose(world=world, local_rotations=local, joint_positions=positions)


def _merge_window_result(
    document: BvhDocument,
    constraints: ConstraintSet,
    existing: FrameResult,
    incoming: FrameResult,
) -> FrameResult:
    pose = _fuse_pose(document, existing.frame, existing.pose, incoming.pose)
    pose_hash = hash_pose(document.joint_names, pose.world)
    residuals, total, satisfied, conflicts, diagnostic = evaluate_residuals(
        document, constraints, existing.frame, pose
    )
    return FrameResult(
        frame=existing.frame,
        pose=pose,
        residuals=residuals,
        total_residual=total,
        satisfied=satisfied,
        conflicts=conflicts,
        pose_hash=pose_hash,
        cache_hit=existing.cache_hit and incoming.cache_hit,
        strategy=existing.strategy,
        iterations=max(existing.iterations, incoming.iterations),
        diagnostic=diagnostic,
    )


def solve_batch(
    document: BvhDocument,
    constraints: ConstraintSet,
    frames: Sequence[int],
    config: SolverConfig,
    cache: SolveCache | None = None,
    progress: Callable[[int, int], None] | None = None,
    cancel_check: Callable[[], None] | None = None,
) -> list[FrameResult]:
    """Solve independent frames in a deterministic parallel batch."""

    frame_list = [int(frame) for frame in frames]
    if not frame_list:
        return []
    workers = min(config.workers, len(frame_list))
    results: list[FrameResult | None] = [None] * len(frame_list)
    completed = 0
    if workers <= 1:
        for index, frame in enumerate(frame_list):
            if cancel_check:
                cancel_check()
            results[index] = solve_frame(
                document, constraints, frame, config, "batch", cache
            )
            completed += 1
            if progress:
                progress(completed, len(frame_list))
    else:
        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="bvh-batch") as pool:
            futures = [
                pool.submit(
                    solve_frame,
                    document,
                    constraints,
                    frame,
                    config,
                    "batch",
                    cache,
                )
                for frame in frame_list
            ]
            for index, future in enumerate(futures):
                results[index] = future.result()
                completed += 1
                if cancel_check:
                    cancel_check()
                if progress:
                    progress(completed, len(frame_list))
    return [item for item in results if item is not None]


def solve_windowed(
    document: BvhDocument,
    constraints: ConstraintSet,
    frames: Sequence[int],
    config: SolverConfig,
    cache: SolveCache | None = None,
    progress: Callable[[int, int], None] | None = None,
    cancel_check: Callable[[], None] | None = None,
) -> list[FrameResult]:
    """Solve sliding windows with overlap fusion.

    Every frame is solved by the same deterministic per-frame routine, so two
    windows containing the same frame normally fuse bit-identical poses.  The
    fusion branch remains meaningful if a future solver adds temporal terms.
    """

    frame_list = [int(frame) for frame in frames]
    if not frame_list:
        return []
    merged: dict[int, FrameResult] = {}
    window_size = min(config.window_size, len(frame_list))
    step = max(1, window_size - config.window_overlap)
    windows: list[list[int]] = []
    start = 0
    while start < len(frame_list):
        end = min(start + window_size, len(frame_list))
        windows.append(frame_list[start:end])
        if end >= len(frame_list):
            break
        start += step
    completed = 0
    for window in windows:
        for frame in window:
            if cancel_check:
                cancel_check()
            current = solve_frame(
                document, constraints, frame, config, "window", cache
            )
            existing = merged.get(frame)
            if existing is None:
                merged[frame] = current
            else:
                merged[frame] = _merge_window_result(
                    document, constraints, existing, current
                )
            completed += 1
            if progress:
                progress(completed, len(frame_list))
    return [merged[frame] for frame in frame_list]


def solve_frames(
    document: BvhDocument,
    constraints: ConstraintSet,
    frames: Sequence[int],
    config: SolverConfig,
    *,
    strategy: str | None = None,
    cache: SolveCache | None = None,
    progress: Callable[[int, int], None] | None = None,
    cancel_check: Callable[[], None] | None = None,
) -> list[FrameResult]:
    constraints.validate(document)
    chosen = strategy or select_strategy(document, constraints, config)
    if chosen == "batch":
        return solve_batch(
            document,
            constraints,
            frames,
            config,
            cache=cache,
            progress=progress,
            cancel_check=cancel_check,
        )
    if chosen == "window":
        return solve_windowed(
            document,
            constraints,
            frames,
            config,
            cache=cache,
            progress=progress,
            cancel_check=cancel_check,
        )
    _config_fail("unknown execution strategy", strategy=chosen)
    return []


def estimate_memory_bytes(
    document: BvhDocument,
    constraints: ConstraintSet,
    frame_count: int,
    config: SolverConfig,
) -> int:
    """Conservative deterministic working-set estimate for a frame interval."""

    joints = max(document.joint_count, 1)
    variables = joints * 3
    pose_bytes = frame_count * joints * 16 * 8
    residual_bytes = frame_count * max(len(constraints.constraints), 1) * 4 * 8
    jacobian_bytes = (
        max(len(constraints.constraints), 1)
        * 3
        * variables
        * 8
        * max(config.max_iterations, 1)
    )
    work_bytes = frame_count * (joints * 9 * 8 + variables * variables * 8)
    return int(pose_bytes + residual_bytes + jacobian_bytes + work_bytes)


def trajectory_checksum(records: Iterable[dict[str, Any]]) -> str:
    h = hashlib.sha256()
    for record in records:
        h.update(canonical_json_bytes(record))
        h.update(b"\n")
    return h.hexdigest()

