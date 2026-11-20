"""Versioned processing engine, incremental invalidation and mode comparison."""

from __future__ import annotations

import math
import os
import threading
from pathlib import Path
from typing import Callable, Dict, Iterable, List, Optional, Sequence, Set, Tuple

import numpy as np

from .errors import (
    E_BUSY,
    E_CANCELLED,
    E_MODE_INVALID,
    WorkbenchError,
    ForcedTermination,
)
from .factors import STEP_NAMES, FactorSet, NoiseParams, ErrorModel
from .model import ImageSequence, SequenceStructure, build_structure
from .pipeline import (
    MODE_FIXED,
    MODE_FLOAT,
    BlockComputer,
    BlockResult,
    StepSummary,
    bilinear_error_bound,
    sample_bilinear,
    validate_mode,
)
from .snapshot import RecoveryState, SnapshotStore
from .util import (
    canonical_json_bytes,
    canonical_json_text,
    sha256_bytes,
    write_bytes_atomic,
    write_text_atomic,
)


ProgressCallback = Callable[[dict], None]
_ACTIVE_LOCK = threading.Lock()
_ACTIVE_SEQUENCES: Set[str] = set()


def _location_key(frame: int, block: int) -> Tuple[int, int]:
    return int(frame), int(block)


def _summary_dict(summary: StepSummary) -> dict:
    return summary.canonical_dict()


class ProcessResult:
    """An immutable published result version plus its assembled diagnostics."""

    def __init__(
        self,
        *,
        version: int,
        mode: str,
        sequence: ImageSequence,
        structure: SequenceStructure,
        factors: FactorSet,
        factor_hash: str,
        blocks: Sequence[BlockResult],
        recomputed_blocks: int,
        reused_blocks: int,
        recovered_blocks: int,
        trace_lines: Sequence[dict],
        summary_diff: dict,
        snapshot_version: int,
    ) -> None:
        self.version = int(version)
        self.mode = mode
        self.sequence_hash = sequence.sequence_hash
        self.structure = structure
        self.factors = factors
        self.factor_hash = factor_hash
        self.snapshot_version = int(snapshot_version)
        self.recomputed_blocks = int(recomputed_blocks)
        self.reused_blocks = int(reused_blocks)
        self.recovered_blocks = int(recovered_blocks)
        self.trace_lines = tuple(trace_lines)
        self.summary_diff = summary_diff
        ordered = sorted(blocks, key=lambda item: item.ordinal)
        if len(ordered) != structure.total_blocks:
            raise RuntimeError("result does not contain every sequence block")
        self.blocks = tuple(ordered)
        self.block_map = {item.ordinal: item for item in ordered}

        pixels = np.empty(
            (structure.frame_count, structure.height, structure.width),
            dtype="<f8",
        )
        errors = np.empty_like(pixels)
        attribution = np.empty_like(pixels, dtype=np.int8)
        for block in ordered:
            ref = structure.block(block.frame_index, block.block_index)
            region = (
                slice(ref.y0, ref.y1),
                slice(ref.x0, ref.x1),
            )
            pixels[block.frame_index][region] = block.pixels
            errors[block.frame_index][region] = block.error_bound
            attribution[block.frame_index][region] = block.attribution
        pixels.setflags(write=False)
        errors.setflags(write=False)
        attribution.setflags(write=False)
        self.pixels = pixels
        self.error_bounds = errors
        self.attribution = attribution
        self.untrusted = errors > factors.error.threshold
        self.untrusted.setflags(write=False)

        self.step_summaries = self._aggregate_step_summaries()
        self.fixed_float_comparison = self._aggregate_mode_comparison()
        self.frame_digests = self._compute_frame_digests()
        self.overall_digest = self._compute_overall_digest()

    def _aggregate_step_summaries(self) -> Dict[str, Tuple[StepSummary, ...]]:
        result: Dict[str, Tuple[StepSummary, ...]] = {}
        for frame in range(self.structure.frame_count):
            block_ids = range(
                frame * self.structure.blocks_per_frame,
                (frame + 1) * self.structure.blocks_per_frame,
            )
            frame_blocks = [self.blocks[index] for index in block_ids]
            for step_index, step_name in enumerate(STEP_NAMES):
                items = [block.step_summaries[step_index] for block in frame_blocks]
                count = sum(item.pixel_count for item in items)
                total = sum(item.sum_error_bound for item in items)
                summary = StepSummary(
                    step=step_name,
                    pixel_count=int(count),
                    max_abs_error=max(item.max_abs_error for item in items),
                    max_error_bound=max(item.max_error_bound for item in items),
                    sum_error_bound=float(total),
                    mean_error_bound=(float(total) / count) if count else 0.0,
                )
                result.setdefault(step_name, []).append(summary)
        return {name: tuple(items) for name, items in result.items()}

    def _aggregate_mode_comparison(self) -> dict:
        per_frame = []
        total_sum = 0.0
        total_count = 0
        total_max = 0.0
        for frame in range(self.structure.frame_count):
            blocks = [
                block
                for block in self.blocks
                if block.frame_index == frame
            ]
            count = sum(block.fixed_float_pixel_count for block in blocks)
            abs_sum = sum(block.fixed_float_abs_sum for block in blocks)
            maximum = max(
                (block.fixed_float_max_abs for block in blocks), default=0.0
            )
            per_frame.append(
                {
                    "frame": frame,
                    "pixel_count": int(count),
                    "mean_abs_diff": (abs_sum / count) if count else 0.0,
                    "max_abs_diff": float(maximum),
                }
            )
            total_sum += abs_sum
            total_count += count
            total_max = max(total_max, maximum)
        return {
            "reference": MODE_FLOAT,
            "candidate": MODE_FIXED,
            "pixel_count": int(total_count),
            "mean_abs_diff": (total_sum / total_count) if total_count else 0.0,
            "max_abs_diff": float(total_max),
            "frames": per_frame,
        }

    def _compute_frame_digests(self) -> Tuple[str, ...]:
        digests: List[str] = []
        for frame in range(self.structure.frame_count):
            digest = hashlib_sha()
            digest.update(self.pixels[frame].tobytes(order="C"))
            digest.update(self.error_bounds[frame].tobytes(order="C"))
            digest.update(self.attribution[frame].tobytes(order="C"))
            for step_name in STEP_NAMES:
                digest.update(
                    canonical_json_bytes(
                        self.step_summaries[step_name][frame].canonical_dict()
                    )
                )
            digests.append(digest.hexdigest())
        return tuple(digests)

    def _compute_overall_digest(self) -> str:
        value = {
            "mode": self.mode,
            "factor_hash": self.factor_hash,
            "sequence_hash": self.sequence_hash,
            "frame_digests": list(self.frame_digests),
            "blocks": [
                {
                    "ordinal": block.ordinal,
                    "frame": block.frame_index,
                    "block": block.block_index,
                    "content_hash": block.content_hash,
                }
                for block in self.blocks
            ],
        }
        return sha256_bytes(canonical_json_bytes(value))

    def frame(self, frame_index: int) -> np.ndarray:
        return self.pixels[frame_index]

    def frame_error(self, frame_index: int) -> np.ndarray:
        return self.error_bounds[frame_index]

    def frame_untrusted(self, frame_index: int) -> np.ndarray:
        return self.untrusted[frame_index]

    def frame_attribution(self, frame_index: int) -> np.ndarray:
        return self.attribution[frame_index]

    def frame_bytes(self, frame_index: int) -> bytes:
        return np.ascontiguousarray(
            self.pixels[frame_index], dtype="<f8"
        ).tobytes(order="C")

    def error_bytes(self, frame_index: int) -> bytes:
        return np.ascontiguousarray(
            self.error_bounds[frame_index], dtype="<f8"
        ).tobytes(order="C")

    def trace_jsonl(self) -> bytes:
        if not self.trace_lines:
            return b""
        return b"".join(
            canonical_json_bytes(item) for item in self.trace_lines
        )

    def canonical_summary(self) -> dict:
        return {
            "mode": self.mode,
            "sequence_hash": self.sequence_hash,
            "factor_hash": self.factor_hash,
            "factor_versions": self.factors.factor_versions(),
            "overall_digest": self.overall_digest,
            "frame_digests": list(self.frame_digests),
            "recomputed_blocks": self.recomputed_blocks,
            "reused_blocks": self.reused_blocks,
            "recovered_blocks": self.recovered_blocks,
            "step_summaries": {
                name: [item.canonical_dict() for item in items]
                for name, items in self.step_summaries.items()
            },
            "fixed_float_comparison": self.fixed_float_comparison,
            "summary_diff": self.summary_diff,
            "untrusted_pixels": int(np.count_nonzero(self.untrusted)),
        }

    def write_outputs(self, directory: os.PathLike) -> None:
        root = Path(directory)
        (root / "frames").mkdir(parents=True, exist_ok=True)
        (root / "errors").mkdir(parents=True, exist_ok=True)
        (root / "attribution").mkdir(parents=True, exist_ok=True)
        (root / "untrusted").mkdir(parents=True, exist_ok=True)
        for frame in range(self.structure.frame_count):
            stem = "frame_{:06d}".format(frame)
            write_bytes_atomic(root / "frames" / (stem + ".f64"), self.frame_bytes(frame))
            write_bytes_atomic(
                root / "errors" / (stem + ".f64"), self.error_bytes(frame)
            )
            write_bytes_atomic(
                root / "attribution" / (stem + ".i8"),
                self.attribution[frame].astype(np.int8).tobytes(order="C"),
            )
            write_bytes_atomic(
                root / "untrusted" / (stem + ".u1"),
                self.untrusted[frame].astype(np.uint8).tobytes(order="C"),
            )
        write_text_atomic(
            root / "result.json",
            canonical_json_text(self.canonical_summary()),
        )
        write_text_atomic(
            root / "summary_diff.json",
            canonical_json_text(self.summary_diff),
        )
        write_bytes_atomic(root / "trace.jsonl", self.trace_jsonl())


def hashlib_sha():
    import hashlib

    return hashlib.sha256()


def _at(array: np.ndarray, frame: int) -> np.ndarray:
    if array.ndim == 2:
        return array
    return array[frame]


class Workbench:
    """Thread-safe coordinator for one image sequence.

    A process call claims the sequence globally.  While it runs, all other
    process submissions for the same sequence content return ``E_BUSY``.
    """

    def __init__(
        self,
        *,
        block_size: int = 64,
        snapshot_root: Optional[os.PathLike] = None,
    ) -> None:
        if block_size <= 0:
            raise ValueError("block_size must be positive")
        self.block_size = int(block_size)
        self.snapshot_root = Path(snapshot_root) if snapshot_root is not None else None
        self._state_lock = threading.RLock()
        self.sequence: Optional[ImageSequence] = None
        self.structure: Optional[SequenceStructure] = None
        self.factors: Optional[FactorSet] = None
        self.mode: Optional[str] = None
        self.result: Optional[ProcessResult] = None
        self.result_version = 0
        self._snapshot_store: Optional[SnapshotStore] = None

    def initialize(
        self,
        sequence: ImageSequence,
        factors: Optional[FactorSet] = None,
        mode: str = MODE_FIXED,
    ) -> None:
        validate_mode(mode)
        structure = build_structure(
            sequence.header.width,
            sequence.header.height,
            sequence.header.frame_count,
            self.block_size,
            data_offset=sequence.header.data_offset,
            frame_stride=sequence.header.frame_stride,
        )
        if factors is None:
            factors = FactorSet.defaults(
                structure.width,
                structure.height,
                structure.block_rows,
                structure.block_cols,
            )
        factors.validate(structure)
        with self._state_lock:
            self.sequence = sequence
            self.structure = structure
            self.factors = factors
            self.mode = mode
            self.result = None
            self.result_version = 0
            if self.snapshot_root is None:
                root = sequence.path.parent / ".sciworkbench_snapshots"
            else:
                root = self.snapshot_root
            root = root / sequence.sequence_hash[:16]
            header_hash = sha256_bytes(sequence.header.to_bytes())
            self._snapshot_store = SnapshotStore(
                root,
                structure,
                sequence.sequence_hash,
                header_hash,
            )

    def _require_initialized(self) -> Tuple[ImageSequence, SequenceStructure, FactorSet, str]:
        with self._state_lock:
            if (
                self.sequence is None
                or self.structure is None
                or self.factors is None
                or self.mode is None
            ):
                raise WorkbenchError(
                    E_MODE_INVALID,
                    "workbench is not initialised with a sequence, factors and mode",
                )
            validate_mode(self.mode)
            return (
                self.sequence,
                self.structure,
                self.factors,
                self.mode,
            )

    def set_factors(self, factors: FactorSet) -> None:
        with self._state_lock:
            if self.structure is None:
                raise WorkbenchError(E_MODE_INVALID, "workbench is not initialised")
            factors.validate(self.structure)
            self.factors = factors

    def set_mode(self, mode: str) -> None:
        validate_mode(mode)
        with self._state_lock:
            if self.sequence is None:
                raise WorkbenchError(E_MODE_INVALID, "workbench is not initialised")
            self.mode = mode

    def update_factor(self, name: str, value) -> None:
        """Replace one influence factor while retaining all others."""

        with self._state_lock:
            if self.factors is None:
                raise WorkbenchError(E_MODE_INVALID, "workbench is not initialised")
            replacement = self.factors.replace(name, value)
            factors = replacement
        self.set_factors(factors)

    @property
    def is_busy(self) -> bool:
        if self.sequence is None:
            return False
        with _ACTIVE_LOCK:
            return self.sequence.sequence_hash in _ACTIVE_SEQUENCES

    def _claim_sequence(self, sequence_hash: str) -> None:
        with _ACTIVE_LOCK:
            if sequence_hash in _ACTIVE_SEQUENCES:
                raise WorkbenchError(
                    E_BUSY,
                    "another processing task is already active for this sequence",
                )
            _ACTIVE_SEQUENCES.add(sequence_hash)

    def _release_sequence(self, sequence_hash: str) -> None:
        with _ACTIVE_LOCK:
            _ACTIVE_SEQUENCES.discard(sequence_hash)

    def _affected_blocks(
        self,
        *,
        old_result: Optional[ProcessResult],
        factors: FactorSet,
        mode: str,
    ) -> Tuple[Set[int], Dict[int, Set[str]]]:
        structure = self.structure
        assert structure is not None
        if old_result is None or old_result.factors is None:
            all_ordinals = set(range(structure.total_blocks))
            return all_ordinals, {
                ordinal: {"initial"} for ordinal in all_ordinals
            }
        affected: Set[int] = set()
        reasons: Dict[int, Set[str]] = {}

        def add_ordinals(ordinals: Iterable[int], reason: str) -> None:
            for ordinal in ordinals:
                affected.add(int(ordinal))
                reasons.setdefault(int(ordinal), set()).add(reason)

        old_mode = old_result.mode
        old_factors = old_result.factors
        if mode != old_mode:
            add_ordinals(range(structure.total_blocks), "mode")

        def compare_spatial(name: str, *, halo_radius: int = 1) -> None:
            before = getattr(old_factors, name)
            after = getattr(factors, name)
            if before.shape != after.shape:
                add_ordinals(range(structure.total_blocks), name)
                return
            for frame in range(structure.frame_count):
                a = _at(before, frame)
                b = _at(after, frame)
                if np.array_equal(a, b):
                    continue
                changed = np.argwhere(a != b)
                if changed.size == 0:
                    continue
                block_ids = set(
                    int(y // structure.block_size) * structure.block_cols
                    + int(x // structure.block_size)
                    for y, x in changed
                )
                ordinals: Set[int] = set()
                for block_id in block_ids:
                    for halo_id in structure.halo_block_ids(
                        block_id, radius=halo_radius
                    ):
                        ordinals.add(
                            frame * structure.blocks_per_frame + halo_id
                        )
                add_ordinals(ordinals, name)

        compare_spatial("dark")
        compare_spatial("flat")
        compare_spatial("bad_mask", halo_radius=2)

        if old_factors.block_gain.shape != factors.block_gain.shape:
            add_ordinals(range(structure.total_blocks), "block_gain")
        else:
            old_gain = sample_bilinear(
                old_factors.block_gain, structure.width, structure.height
            )
            new_gain = sample_bilinear(
                factors.block_gain, structure.width, structure.height
            )
            old_gain_error = bilinear_error_bound(
                old_factors.block_gain, structure.width, structure.height
            )
            new_gain_error = bilinear_error_bound(
                factors.block_gain, structure.width, structure.height
            )
            changed = (old_gain != new_gain) | (old_gain_error != new_gain_error)
            if np.any(changed):
                block_ids = set(
                    int(y // structure.block_size) * structure.block_cols
                    + int(x // structure.block_size)
                    for y, x in np.argwhere(changed)
                )
                for frame in range(structure.frame_count):
                    ordinals: Set[int] = set()
                    for block_id in block_ids:
                        for halo_id in structure.halo_block_ids(block_id, radius=1):
                            ordinals.add(
                                frame * structure.blocks_per_frame + halo_id
                            )
                    add_ordinals(ordinals, "block_gain")

        if old_factors.noise != factors.noise:
            add_ordinals(range(structure.total_blocks), "noise")
        if old_factors.error != factors.error:
            add_ordinals(range(structure.total_blocks), "error")
        if old_factors.frac_bits != factors.frac_bits:
            add_ordinals(range(structure.total_blocks), "frac_bits")
        return affected, reasons

    def _trace_lines(
        self,
        *,
        block_results: Sequence[BlockResult],
        actions: Dict[int, str],
        reasons: Dict[int, Set[str]],
        recomputed: int,
        reused: int,
        recovered: int,
        factors: FactorSet,
        mode: str,
        factor_hash: str,
    ) -> Tuple[dict, ...]:
        lines = []
        for block in sorted(block_results, key=lambda item: item.ordinal):
            lines.append(
                {
                    "frame": int(block.frame_index),
                    "block": int(block.block_index),
                    "action": actions.get(block.ordinal, "reuse"),
                    "factors_changed": sorted(reasons.get(block.ordinal, set())),
                    "factor_hash": factor_hash,
                    "factor_versions": block.factor_versions,
                    "mode": mode,
                    "recomputed_blocks": int(recomputed),
                    "reused_blocks": int(reused),
                    "recovered_blocks": int(recovered),
                    "max_error_bound": block.max_error_bound(),
                    "content_hash": block.content_hash,
                }
            )
        return tuple(lines)

    def _summary_diff(
        self,
        old_result: Optional[ProcessResult],
        new_result: ProcessResult,
    ) -> dict:
        old_frac = old_result.factors.frac_bits if old_result is not None else None
        entries = []
        changed_any = False
        for step_name in STEP_NAMES:
            new_items = new_result.step_summaries[step_name]
            old_items = (
                old_result.step_summaries.get(step_name)
                if old_result is not None
                else None
            )
            for frame, new_item in enumerate(new_items):
                old_item = (
                    old_items[frame]
                    if old_items is not None and frame < len(old_items)
                    else None
                )
                old_value = old_item.canonical_dict() if old_item is not None else None
                new_value = new_item.canonical_dict()
                if old_value is not None and old_value != new_value:
                    changed_any = True
                entries.append(
                    {
                        "step": step_name,
                        "frame": int(frame),
                        "old": old_value,
                        "new": new_value,
                        "delta_max_abs_error": (
                            new_item.max_abs_error - old_item.max_abs_error
                            if old_item is not None
                            else None
                        ),
                        "delta_error_bound": (
                            new_item.max_error_bound - old_item.max_error_bound
                            if old_item is not None
                            else None
                        ),
                    }
                )
        return {
            "frac_bits_old": old_frac,
            "frac_bits_new": new_result.factors.frac_bits,
            "frac_bits_changed": old_frac != new_result.factors.frac_bits,
            "summary_changed": changed_any,
            "steps": entries,
        }

    def _build_result(
        self,
        *,
        sequence: ImageSequence,
        structure: SequenceStructure,
        factors: FactorSet,
        mode: str,
        factor_hash: str,
        blocks: Sequence[BlockResult],
        actions: Dict[int, str],
        reasons: Dict[int, Set[str]],
        recomputed: int,
        reused: int,
        recovered: int,
        snapshot_version: int,
        old_result: Optional[ProcessResult],
    ) -> ProcessResult:
        next_version = self.result_version + 1
        trace = self._trace_lines(
            block_results=blocks,
            actions=actions,
            reasons=reasons,
            recomputed=recomputed,
            reused=reused,
            recovered=recovered,
            factors=factors,
            mode=mode,
            factor_hash=factor_hash,
        )
        result = ProcessResult(
            version=next_version,
            mode=mode,
            sequence=sequence,
            structure=structure,
            factors=factors,
            factor_hash=factor_hash,
            blocks=blocks,
            recomputed_blocks=recomputed,
            reused_blocks=reused,
            recovered_blocks=recovered,
            trace_lines=trace,
            summary_diff={},
            snapshot_version=snapshot_version,
        )
        result.summary_diff = self._summary_diff(old_result, result)
        return result

    def process(
        self,
        *,
        mode: Optional[str] = None,
        cancel_event: Optional[threading.Event] = None,
        progress_callback: Optional[ProgressCallback] = None,
        resume: bool = False,
        interrupt_after_commits: Optional[int] = None,
    ) -> ProcessResult:
        sequence, structure, factors, current_mode = self._require_initialized()
        selected_mode = current_mode if mode is None else validate_mode(mode)
        factors.validate(structure)
        old_result = self.result
        factor_hash = factors.factor_hash(selected_mode)
        store = self._snapshot_store
        assert store is not None

        self._claim_sequence(sequence.sequence_hash)
        try:
            recovery: Optional[RecoveryState] = None
            if resume:
                recovery = store.load_latest(
                    expected_mode=selected_mode,
                    expected_factor_hash=factor_hash,
                )
                if recovery is not None and not recovery.matches_current:
                    recovery = None

            if recovery is not None and recovery.complete:
                actions = {
                    block.ordinal: "recovered" for block in recovery.blocks
                }
                result = self._build_result(
                    sequence=sequence,
                    structure=structure,
                    factors=factors,
                    mode=selected_mode,
                    factor_hash=factor_hash,
                    blocks=recovery.blocks,
                    actions=actions,
                    reasons={},
                    recomputed=0,
                    reused=0,
                    recovered=len(recovery.blocks),
                    snapshot_version=recovery.version,
                    old_result=old_result,
                )
                with self._state_lock:
                    self.result = result
                    self.result_version = result.version
                    self.mode = selected_mode
                return result

            if recovery is None:
                store.create_version(
                    mode=selected_mode,
                    factor_hash=factor_hash,
                    factor_versions=factors.factor_versions(),
                    block_size=self.block_size,
                )
            else:
                store.chain_head = recovery.chain_head

            affected, reasons = self._affected_blocks(
                old_result=old_result,
                factors=factors,
                mode=selected_mode,
            )
            recovered_prefix = {
                block.ordinal: block for block in (recovery.blocks if recovery else ())
            }
            old_blocks = (
                {block.ordinal: block for block in old_result.blocks}
                if old_result is not None
                else {}
            )
            computer: Optional[BlockComputer] = None
            blocks: List[BlockResult] = []
            actions: Dict[int, str] = {}
            recomputed = 0
            reused = 0
            recovered = 0
            commits_this_call = 0
            for index, ref in enumerate(structure.blocks):
                if cancel_event is not None and cancel_event.is_set():
                    raise WorkbenchError(
                        E_CANCELLED,
                        "processing was cancelled; last committed snapshot was retained",
                        frame=ref.frame_index,
                        block=ref.block_index,
                    )
                if index in recovered_prefix:
                    block = recovered_prefix[index]
                    store.skip_committed()
                    actions[index] = "recovered"
                    recovered += 1
                else:
                    if index not in affected and index in old_blocks:
                        block = old_blocks[index]
                        actions[index] = "reuse"
                        reused += 1
                    else:
                        if computer is None:
                            computer = BlockComputer(
                                structure, factors, selected_mode, sequence
                            )
                        block = computer.run_block(ref)
                        actions[index] = "recompute"
                        recomputed += 1
                    block_versions = factors.block_factor_versions(
                        structure, ref.frame_index, ref.block_index
                    )
                    store.commit_block(
                        block,
                        factor_hash=factor_hash,
                        factor_versions=block_versions,
                    )
                    commits_this_call += 1
                blocks.append(block)
                if progress_callback is not None:
                    progress_callback(
                        {
                            "frame": ref.frame_index,
                            "block": ref.block_index,
                            "completed": index + 1,
                            "total": structure.total_blocks,
                            "action": actions[index],
                            "recomputed_blocks": recomputed,
                            "reused_blocks": reused,
                            "recovered_blocks": recovered,
                        }
                    )
                if (
                    interrupt_after_commits is not None
                    and commits_this_call >= interrupt_after_commits
                ):
                    raise ForcedTermination(commits_this_call)
            if cancel_event is not None and cancel_event.is_set():
                raise WorkbenchError(
                    E_CANCELLED,
                    "processing was cancelled before publication",
                )
            store.finalize()
            result = self._build_result(
                sequence=sequence,
                structure=structure,
                factors=factors,
                mode=selected_mode,
                factor_hash=factor_hash,
                blocks=blocks,
                actions=actions,
                reasons=reasons,
                recomputed=recomputed,
                reused=reused,
                recovered=recovered,
                snapshot_version=store.manifest.version if store.manifest else 0,
                old_result=old_result,
            )
            with self._state_lock:
                self.result = result
                self.result_version = result.version
                self.mode = selected_mode
            return result
        finally:
            self._release_sequence(sequence.sequence_hash)

    def recover(
        self,
        *,
        cancel_event: Optional[threading.Event] = None,
        progress_callback: Optional[ProgressCallback] = None,
    ) -> ProcessResult:
        return self.process(
            resume=True,
            cancel_event=cancel_event,
            progress_callback=progress_callback,
        )

    def compare_modes(
        self,
        *,
        cancel_event: Optional[threading.Event] = None,
    ) -> dict:
        sequence, structure, factors, _ = self._require_initialized()
        factors.validate(structure)
        sequences_hash = sequence.sequence_hash
        self._claim_sequence(sequences_hash)
        try:
            fixed_computer = BlockComputer(
                structure, factors, MODE_FIXED, sequence
            )
            float_computer = BlockComputer(
                structure, factors, MODE_FLOAT, sequence
            )
            fixed_frame = np.empty(
                (structure.frame_count, structure.height, structure.width),
                dtype=np.float64,
            )
            fixed_error = np.empty_like(fixed_frame)
            float_frame = np.empty_like(fixed_frame)
            float_error = np.empty_like(fixed_frame)
            fixed_attr = np.empty(
                (structure.frame_count, structure.height, structure.width),
                dtype=np.int8,
            )
            float_attr = np.empty_like(fixed_attr)
            for ref in structure.blocks:
                if cancel_event is not None and cancel_event.is_set():
                    raise WorkbenchError(
                        E_CANCELLED,
                        "mode comparison was cancelled",
                        frame=ref.frame_index,
                        block=ref.block_index,
                    )
                fixed_block = fixed_computer.run_block(ref)
                float_block = float_computer.run_block(ref)
                region = (
                    slice(ref.y0, ref.y1),
                    slice(ref.x0, ref.x1),
                )
                fixed_frame[ref.frame_index][region] = fixed_block.pixels
                fixed_error[ref.frame_index][region] = fixed_block.error_bound
                fixed_attr[ref.frame_index][region] = fixed_block.attribution
                float_frame[ref.frame_index][region] = float_block.pixels
                float_error[ref.frame_index][region] = float_block.error_bound
                float_attr[ref.frame_index][region] = float_block.attribution

            frame_reports = []
            all_abs = []
            all_error_delta = []
            for frame in range(structure.frame_count):
                absolute = np.abs(fixed_frame[frame] - float_frame[frame])
                error_delta = fixed_error[frame] - float_error[frame]
                all_abs.append(absolute.ravel())
                all_error_delta.append(error_delta.ravel())
                frame_reports.append(
                    {
                        "frame": frame,
                        "pixel_count": int(absolute.size),
                        "mean_abs_diff": float(np.mean(absolute)),
                        "max_abs_diff": float(np.max(absolute)),
                        "rms_abs_diff": float(
                            math.sqrt(float(np.mean(absolute * absolute)))
                        ),
                        "p95_abs_diff": float(np.quantile(absolute, 0.95)),
                        "fixed_error_max": float(np.max(fixed_error[frame])),
                        "float_error_max": float(np.max(float_error[frame])),
                        "error_bound_delta_mean": float(np.mean(error_delta)),
                        "error_bound_delta_max": float(np.max(error_delta)),
                        "fixed_frame_sha256": sha256_bytes(
                            np.ascontiguousarray(
                                fixed_frame[frame], dtype="<f8"
                            ).tobytes(order="C")
                        ),
                        "float_frame_sha256": sha256_bytes(
                            np.ascontiguousarray(
                                float_frame[frame], dtype="<f8"
                            ).tobytes(order="C")
                        ),
                        "fixed_untrusted": int(
                            np.count_nonzero(fixed_error[frame] > factors.error.threshold)
                        ),
                        "float_untrusted": int(
                            np.count_nonzero(float_error[frame] > factors.error.threshold)
                        ),
                    }
                )
            absolute_all = np.concatenate(all_abs) if all_abs else np.array([])
            error_delta_all = (
                np.concatenate(all_error_delta)
                if all_error_delta
                else np.array([])
            )
            report = {
                "schema": 1,
                "sequence_hash": sequences_hash,
                "factor_hash": factors.factor_hash("fixed+float"),
                "operator_semantics": "identical",
                "reference_mode": MODE_FLOAT,
                "candidate_mode": MODE_FIXED,
                "pixel_count": int(absolute_all.size),
                "mean_abs_diff": float(np.mean(absolute_all)),
                "max_abs_diff": float(np.max(absolute_all)),
                "rms_abs_diff": float(
                    math.sqrt(float(np.mean(absolute_all * absolute_all)))
                ),
                "p95_abs_diff": float(np.quantile(absolute_all, 0.95)),
                "error_bound_delta_mean": float(np.mean(error_delta_all)),
                "error_bound_delta_max": float(np.max(error_delta_all)),
                "fixed_digest": sha256_bytes(
                    np.ascontiguousarray(fixed_frame, dtype="<f8").tobytes(order="C")
                ),
                "float_digest": sha256_bytes(
                    np.ascontiguousarray(float_frame, dtype="<f8").tobytes(order="C")
                ),
                "fixed_attribution_digest": sha256_bytes(
                    np.ascontiguousarray(fixed_attr, dtype=np.int8).tobytes(order="C")
                ),
                "float_attribution_digest": sha256_bytes(
                    np.ascontiguousarray(float_attr, dtype=np.int8).tobytes(order="C")
                ),
                "frames": frame_reports,
            }
            return report
        finally:
            self._release_sequence(sequences_hash)
