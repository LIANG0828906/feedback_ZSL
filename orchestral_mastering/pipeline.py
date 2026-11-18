from __future__ import annotations

import hashlib
import json
import math
import os
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

import numpy as np
import soundfile as sf

from .alignment import AlignmentResult, align_stems, realign_from_params
from .dsp import (
    apply_loudness_and_peak_limit,
    db,
    linked_true_peak_limiter,
    pan_weights,
    stable_float,
)
from .errors import BusinessError, Issue
from .manifest import Manifest, load_manifest
from .snapshot import (
    atomic_write_bytes,
    atomic_write_json,
    block_path,
    block_sha256,
    clear_progress,
)
from .unmask import UnmaskResult, reapply_unmask_params, unmask_stems

ENGINE_VERSION = "1.0.0"
SNAPSHOT_SCHEMA = 1


@dataclass
class RenderConfig:
    manifest_path: Path
    output_path: Path
    report_path: Path
    snapshot_path: Path
    block_frames: int | None = None
    fresh: bool = False
    block_delay_seconds: float = 0.0
    stop_requested: Callable[[], bool] | None = None

    @property
    def blocks_dir(self) -> Path:
        return self.snapshot_path.parent / f"{self.snapshot_path.stem}_blocks"


@dataclass
class BalanceResult:
    raw_stereo: np.ndarray
    gains: dict[str, float]
    pan: dict[str, tuple[float, float]]
    track_energies: dict[str, float]
    target_ratios: dict[str, float]
    pre_ratios: dict[str, float]
    params: dict[str, Any]
    warnings: list[dict[str, Any]] = field(default_factory=list)

    def report(self) -> dict[str, Any]:
        tracks = []
        for role, target in self.target_ratios.items():
            gain = self.gains[role]
            tracks.append(
                {
                    "role": role,
                    "target_ratio": stable_float(target),
                    "pre_balance_ratio": stable_float(self.pre_ratios.get(role, 0.0)),
                    "gain_linear": stable_float(gain),
                    "gain_db": stable_float(db(gain)),
                    "pan_left": stable_float(self.pan[role][0]),
                    "pan_right": stable_float(self.pan[role][1]),
                    "contribution_ratio": stable_float(self.pre_ratios.get(role, 0.0)),
                }
            )
        deviation = max(
            (
                abs(self.pre_ratios.get(item["role"], 0.0) - self.target_ratios[item["role"]])
                for item in tracks
            ),
            default=0.0,
        )
        return {
            "tracks": tracks,
            "max_ratio_deviation": stable_float(deviation),
        }


@dataclass
class MasterResultBundle:
    result: Any
    raw_stereo: np.ndarray
    track_contributions: dict[str, np.ndarray]
    contribution_ratios: dict[str, float]
    max_ratio_deviation: float
    params: dict[str, Any]
    report: dict[str, Any]


def _load_audio(path: Path) -> np.ndarray:
    data, _ = sf.read(str(path), dtype="float64", always_2d=False)
    return np.asarray(data, dtype=np.float64).reshape(-1)


def _load_sources(manifest: Manifest) -> tuple[dict[str, np.ndarray], dict[str, np.ndarray]]:
    stems: dict[str, np.ndarray] = {}
    references: dict[str, np.ndarray] = {}
    for track in manifest.tracks:
        stems[track.role] = _load_audio(track.path)
        references[track.role] = _load_audio(track.reference_path)
    return stems, references


def _target_total_energy(master_frames: int) -> float:
    return 0.01 * master_frames


def balance_stems(
    manifest: Manifest,
    unmasked: UnmaskResult,
    alignment: AlignmentResult,
    *,
    role_gain_overrides: dict[str, float] | None = None,
) -> BalanceResult:
    master_frames = alignment.master_frames
    track_energy = {
        track.role: float(np.sum(unmasked.stems[track.role] ** 2))
        for track in manifest.tracks
    }
    ratios = {track.role: track.target_energy_ratio for track in manifest.tracks}
    total_target = _target_total_energy(master_frames)
    gains: dict[str, float] = {}
    pans: dict[str, tuple[float, float]] = {}
    for track in manifest.tracks:
        pans[track.role] = pan_weights(track.anchor[0])
        if role_gain_overrides is not None and track.role in role_gain_overrides:
            gains[track.role] = float(role_gain_overrides[track.role])
        elif track_energy[track.role] <= 1e-24 or ratios[track.role] == 0:
            gains[track.role] = 0.0
        else:
            gains[track.role] = math.sqrt(
                ratios[track.role] * total_target / track_energy[track.role]
            )
    contributions: dict[str, np.ndarray] = {}
    raw = np.zeros((master_frames, 2), dtype=np.float64)
    for track in manifest.tracks:
        audio = unmasked.stems[track.role]
        padded = np.zeros(master_frames, dtype=np.float64)
        padded[: min(len(audio), master_frames)] = audio[:master_frames]
        left, right = pans[track.role]
        contribution = np.stack(
            (padded * gains[track.role] * left, padded * gains[track.role] * right),
            axis=1,
        )
        contributions[track.role] = contribution
        raw += contribution
    contribution_energy = {
        role: float(np.sum(value**2)) for role, value in contributions.items()
    }
    total_energy = max(1e-30, sum(contribution_energy.values()))
    pre_ratios = {
        role: contribution_energy[role] / total_energy for role in contributions
    }
    params = {
        # Full precision: this state is replayed after a forced termination.
        "gains": {role: float(value) for role, value in gains.items()},
        "pan": {
            role: [stable_float(value[0]), stable_float(value[1])]
            for role, value in pans.items()
        },
        "target_ratios": {role: stable_float(value) for role, value in ratios.items()},
        "track_energies": {role: float(value) for role, value in track_energy.items()},
        "master_frames": master_frames,
        "total_target_energy": stable_float(total_target),
    }
    return BalanceResult(
        raw_stereo=raw,
        gains=gains,
        pan=pans,
        track_energies=track_energy,
        target_ratios=ratios,
        pre_ratios=pre_ratios,
        params=params,
        warnings=[],
    )


def _reconstruct_balance(
    manifest: Manifest,
    unmasked: UnmaskResult,
    alignment: AlignmentResult,
    params: dict[str, Any],
) -> BalanceResult:
    gains_by_role = params.get("gains", {})
    if set(gains_by_role) != {track.role for track in manifest.tracks}:
        raise ValueError("快照中的声部增益集合与清单不一致")
    return balance_stems(
        manifest,
        unmasked,
        alignment,
        role_gain_overrides={role: float(value) for role, value in gains_by_role.items()},
    )


def _contribution_ratios_after_limiter(
    contributions: dict[str, np.ndarray],
    pre_gain: float,
    limiter_gain: np.ndarray,
) -> dict[str, float]:
    energies: dict[str, float] = {}
    for role, contribution in contributions.items():
        scaled = contribution * (pre_gain * limiter_gain[:, None])
        energies[role] = float(np.sum(scaled**2))
    total = max(1e-30, sum(energies.values()))
    return {role: energy / total for role, energy in energies.items()}


def _master_from_pregain(
    raw: np.ndarray,
    sample_rate: int,
    pre_gain: float,
    true_peak_limit_dbtp: float,
) -> Any:
    gained = raw * pre_gain
    limited = linked_true_peak_limiter(
        gained,
        sample_rate,
        true_peak_limit_dbtp - 0.02,
        safety_db=0.06,
    )
    return limited


def _master_report(
    manifest: Manifest,
    master: Any,
    ratio_deviation: float,
) -> dict[str, Any]:
    audio = master.audio
    peak_left = float(np.max(np.abs(audio[:, 0]))) if len(audio) else 0.0
    peak_right = float(np.max(np.abs(audio[:, 1]))) if len(audio) else 0.0
    return {
        "status": "complete",
        "frames": len(audio),
        "sample_rate": manifest.sample_rate,
        "target_loudness_lufs": stable_float(master.target_loudness_lufs),
        "loudness_lufs_after_limiter": stable_float(master.loudness_after_limiter),
        "loudness_lufs_before_limiter": stable_float(master.loudness_before_limiter),
        "loudness_error_lu": stable_float(
            master.loudness_after_limiter - master.target_loudness_lufs
        ),
        "target_true_peak_dbtp": stable_float(master.target_true_peak_dbtp),
        "true_peak_dbtp_before_limiter": stable_float(master.true_peak_before_limiter),
        "true_peak_dbtp_after_limiter": stable_float(master.true_peak_after_limiter),
        "peak_dbfs_left": stable_float(db(peak_left)),
        "peak_dbfs_right": stable_float(db(peak_right)),
        "limiter_triggered": bool(master.limiter.triggered),
        "limiter_triggered_samples": int(master.limiter.triggered_frames),
        "limiter_triggered_frames": int(master.limiter.triggered_frames),
        "limiter_latency_compensation_samples": int(master.limiter.latency_samples),
        "pre_gain_linear": stable_float(master.pre_gain),
        "pre_gain_db": stable_float(db(master.pre_gain)),
        "loudness_iterations": int(master.iterations),
        "max_ratio_deviation_after_limiter": stable_float(ratio_deviation),
    }


def _build_master_bundle(
    manifest: Manifest,
    balance: BalanceResult,
    unmasked: UnmaskResult,
    alignment: AlignmentResult,
) -> MasterResultBundle:
    contributions: dict[str, np.ndarray] = {}
    master_frames = alignment.master_frames
    for track in manifest.tracks:
        audio = unmasked.stems[track.role]
        padded = np.zeros(master_frames, dtype=np.float64)
        padded[: min(len(audio), master_frames)] = audio[:master_frames]
        left, right = balance.pan[track.role]
        gain = balance.gains[track.role]
        contributions[track.role] = np.stack(
            (padded * gain * left, padded * gain * right), axis=1
        )
    raw = np.sum(np.stack(list(contributions.values()), axis=0), axis=0)
    master = apply_loudness_and_peak_limit(
        raw,
        manifest.sample_rate,
        manifest.target_loudness_lufs,
        manifest.true_peak_max_dbtp,
    )
    ratios = _contribution_ratios_after_limiter(contributions, master.pre_gain, master.limiter_gain)
    ratio_deviation = max(
        (abs(ratios[track.role] - track.target_energy_ratio) for track in manifest.tracks),
        default=0.0,
    )
    correction_iterations = 0
    if ratio_deviation > 0.02:
        # If the linked limiter slightly tilts the ratios, solve the energy
        # equations against the observed post-limiter ratios.  Repeat only when
        # limiting actually changed the temporal balance.
        adjusted = dict(balance.gains)
        for _ in range(4):
            correction_iterations += 1
            for track in manifest.tracks:
                observed = max(ratios[track.role], 1e-9)
                desired = max(track.target_energy_ratio, 0.0)
                if observed > 0 and desired > 0:
                    adjusted[track.role] *= math.sqrt(desired / observed)
            balance = balance_stems(
                manifest,
                unmasked,
                alignment,
                role_gain_overrides=adjusted,
            )
            contributions = {}
            for track in manifest.tracks:
                audio = unmasked.stems[track.role]
                padded = np.zeros(master_frames, dtype=np.float64)
                padded[: min(len(audio), master_frames)] = audio[:master_frames]
                left, right = balance.pan[track.role]
                gain = balance.gains[track.role]
                contributions[track.role] = np.stack(
                    (padded * gain * left, padded * gain * right), axis=1
                )
            raw = np.sum(np.stack(list(contributions.values()), axis=0), axis=0)
            master = apply_loudness_and_peak_limit(
                raw,
                manifest.sample_rate,
                manifest.target_loudness_lufs,
                manifest.true_peak_max_dbtp,
            )
            ratios = _contribution_ratios_after_limiter(
                contributions, master.pre_gain, master.limiter_gain
            )
            ratio_deviation = max(
                (abs(ratios[track.role] - track.target_energy_ratio) for track in manifest.tracks),
                default=0.0,
            )
            if ratio_deviation <= 0.02:
                break
    report = _master_report(
        manifest,
        master,
        ratio_deviation,
    )
    report["ratio_correction_iterations"] = correction_iterations
    report["group_contribution_ratios"] = {
        role: stable_float(value) for role, value in ratios.items()
    }
    params = {
        "pre_gain": float(master.pre_gain),
        "true_peak_limit_dbtp": float(manifest.true_peak_max_dbtp),
        "lookahead_ms": 3.0,
        "release_ms": 70.0,
        "safety_db": 0.06,
        "target_loudness_lufs": float(manifest.target_loudness_lufs),
        "report": report,
    }
    return MasterResultBundle(
        result=master,
        raw_stereo=raw,
        track_contributions=contributions,
        contribution_ratios=ratios,
        max_ratio_deviation=ratio_deviation,
        params=params,
        report=report,
    )


def _make_report(
    manifest: Manifest,
    alignment: AlignmentResult,
    unmasked: UnmaskResult,
    balance: BalanceResult,
    master: MasterResultBundle,
    *,
    snapshot: dict[str, Any],
    status: str,
    warnings_list: list[dict[str, Any]],
    output_path: Path,
) -> dict[str, Any]:
    loudness_ok = abs(float(master.report.get("loudness_error_lu", 999.0))) <= 0.5
    true_peak_ok = float(master.report.get("true_peak_dbtp_after_limiter", 0.0)) <= (
        manifest.true_peak_max_dbtp + 1e-6
    )
    ratio_ok = float(master.report.get("max_ratio_deviation_after_limiter", 1.0)) <= 0.02
    alignment_ok = all(
        (not item.detected)
        or (
            item.alignment_error_samples is not None
            and item.alignment_error_samples <= 1
        )
        for item in alignment.stems.values()
    )
    unmask_ok = all(
        (not item.detected) or item.attenuation_db >= manifest.min_attenuation_db
        for item in unmasked.channels
    )
    report = {
        "ok": True,
        "schema_version": 1,
        "engine_version": ENGINE_VERSION,
        "status": status,
        "manifest": {
            "path": str(manifest.path),
            "sha256": manifest.raw_sha256,
        },
        "sample_rate": manifest.sample_rate,
        "bit_depth": manifest.bit_depth,
        "channels": 2,
        "output_path": str(output_path),
        "alignment": alignment.report(),
        "unmasking": unmasked.report(manifest),
        "balance": balance.report(),
        "master": {
            **master.report,
            "target_ratio_deviation": stable_float(master.max_ratio_deviation),
        },
        "compliance": {
            "alignment_within_one_sample": alignment_ok,
            "unmask_attenuation_met": unmask_ok,
            "balance_ratio_within_0_02": ratio_ok,
            "loudness_within_0_5_lu": loudness_ok,
            "true_peak_within_limit": true_peak_ok,
            "overall": bool(
                alignment_ok and unmask_ok and ratio_ok and loudness_ok and true_peak_ok
            ),
        },
        "snapshot": {
            "path": str(snapshot["_path"]),
            "block_frames": snapshot["block_frames"],
            "total_blocks": snapshot["total_blocks"],
            "completed_blocks": snapshot["completed_blocks"],
            "commits": snapshot["commit_count"],
            "commit_count": snapshot["commit_count"],
            "status": snapshot["status"],
        },
        "warnings": warnings_list,
        "errors": [],
    }
    return report


def _snapshot_base(manifest: Manifest, config: RenderConfig, block_frames: int) -> dict[str, Any]:
    return {
        "schema_version": SNAPSHOT_SCHEMA,
        "engine_version": ENGINE_VERSION,
        "manifest_sha256": manifest.raw_sha256,
        "input_sha256": manifest.input_digest(),
        "block_frames": block_frames,
        "total_frames": 0,
        "total_blocks": 0,
        "completed_blocks": 0,
        "commit_count": 0,
        "status": "rendering",
        "blocks": [],
        "alignment_params": {},
        "unmask_params": {},
        "balance_params": {},
        "master_params": {},
        "_path": str(config.snapshot_path),
    }


def _public_snapshot(snapshot: dict[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in snapshot.items() if not key.startswith("_")}


def _read_snapshot(path: Path, blocks_dir: Path) -> dict[str, Any] | None:
    if not path.is_file():
        return None
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        clear_progress(path, blocks_dir)
        raise BusinessError(
            Issue(
                "E_RESUME_MISMATCH",
                f"快照无法读取: {exc}",
                channel="snapshot",
                details={"path": str(path)},
            ),
            exit_code=3,
        ) from exc
    if not isinstance(value, dict):
        clear_progress(path, blocks_dir)
        raise BusinessError(
            Issue("E_RESUME_MISMATCH", "快照根节点不是对象", channel="snapshot", details={"path": str(path)}),
            exit_code=3,
        )
    return value


def _mismatch(
    config: RenderConfig,
    manifest: Manifest,
    reason: str,
    details: dict[str, Any] | None = None,
) -> BusinessError:
    clear_progress(config.snapshot_path, config.blocks_dir)
    return BusinessError(
        Issue(
            "E_RESUME_MISMATCH",
            f"已丢弃旧进度: {reason}",
            channel="snapshot",
            details={"manifest_sha256": manifest.raw_sha256, **(details or {})},
        ),
        exit_code=3,
    )


def _validate_snapshot_header(
    snapshot: dict[str, Any],
    manifest: Manifest,
    config: RenderConfig,
    block_frames: int,
) -> None:
    if snapshot.get("schema_version") != SNAPSHOT_SCHEMA:
        raise _mismatch(config, manifest, "快照 schema_version 不一致")
    if snapshot.get("engine_version") != ENGINE_VERSION:
        raise _mismatch(config, manifest, "快照引擎版本不一致")
    if snapshot.get("status") not in {"rendering", "interrupted", "complete"}:
        raise _mismatch(config, manifest, "快照状态非法")
    if snapshot.get("manifest_sha256") != manifest.raw_sha256:
        raise _mismatch(config, manifest, "清单摘要不一致")
    if snapshot.get("input_sha256") != manifest.input_digest():
        raise _mismatch(config, manifest, "输入音频摘要不一致")
    if int(snapshot.get("block_frames", -1)) != block_frames:
        raise _mismatch(config, manifest, "固定采样块大小不一致")


def _verify_blocks(
    snapshot: dict[str, Any],
    config: RenderConfig,
    manifest: Manifest,
    total_frames: int,
) -> None:
    completed = int(snapshot.get("completed_blocks", 0))
    blocks = snapshot.get("blocks", [])
    if completed < 0 or completed > int(snapshot.get("total_blocks", 0)):
        raise _mismatch(config, manifest, "已完成块计数非法")
    if len(blocks) != completed:
        raise _mismatch(config, manifest, "块清单长度与已完成块数不一致")
    block_frames = int(snapshot["block_frames"])
    for index, item in enumerate(blocks):
        if int(item.get("index", -1)) != index:
            raise _mismatch(config, manifest, "块索引不连续", {"block": index})
        path = block_path(config.blocks_dir, index)
        if not path.is_file():
            raise _mismatch(config, manifest, "已提交块文件缺失", {"block": index})
        if block_sha256(path) != item.get("sha256"):
            raise _mismatch(config, manifest, "已提交块文件摘要不一致", {"block": index})
        expected = block_frames
        if index == int(snapshot.get("total_blocks", 0)) - 1:
            expected = total_frames - block_frames * index
        if int(item.get("frames", -1)) != expected:
            raise _mismatch(config, manifest, "块帧数与快照不一致", {"block": index})


def _validate_determined_params(
    manifest: Manifest,
    snapshot: dict[str, Any],
    config: RenderConfig,
) -> None:
    stems, references = _load_sources(manifest)
    fresh_alignment = align_stems(manifest, stems, references)
    fresh_unmask = unmask_stems(manifest, fresh_alignment)
    for key, fresh in (
        ("alignment_params", fresh_alignment.params),
        ("unmask_params", fresh_unmask.params),
    ):
        expected = json.dumps(fresh, sort_keys=True, separators=(",", ":"))
        stored = json.dumps(snapshot.get(key, {}), sort_keys=True, separators=(",", ":"))
        if expected != stored:
            label = "瞬态偏移" if key == "alignment_params" else "解耦参数"
            raise _mismatch(config, manifest, f"已确定{label}与当前输入不一致")


def _write_output_from_blocks(
    manifest: Manifest,
    config: RenderConfig,
    snapshot: dict[str, Any],
    total_frames: int,
) -> np.ndarray:
    block_frames = int(snapshot["block_frames"])
    output = np.empty((total_frames, 2), dtype=np.float32)
    offset = 0
    for index, item in enumerate(snapshot["blocks"]):
        path = block_path(config.blocks_dir, index)
        raw = path.read_bytes()
        array = np.frombuffer(raw, dtype="<f4").reshape(-1, 2)
        if len(array) != int(item["frames"]):
            raise _mismatch(config, manifest, "块文件帧数不一致", {"block": index})
        output[offset : offset + len(array)] = array
        offset += len(array)
    if offset != total_frames:
        raise _mismatch(config, manifest, "已提交块总帧数不等于母带长度")
    temporary = config.output_path.with_name(config.output_path.name + ".tmp")
    config.output_path.parent.mkdir(parents=True, exist_ok=True)
    subtype = {16: "PCM_16", 24: "PCM_24", 32: "PCM_32"}[manifest.bit_depth]
    sf.write(
        str(temporary),
        output,
        manifest.sample_rate,
        format="WAV",
        subtype=subtype,
    )
    os.replace(temporary, config.output_path)
    return output.astype(np.float64)


def _write_report(path: Path, report: dict[str, Any]) -> None:
    atomic_write_json(path, report)


def _block_array(audio: np.ndarray, index: int, block_frames: int) -> np.ndarray:
    start = index * block_frames
    end = min(len(audio), start + block_frames)
    return np.asarray(audio[start:end], dtype="<f4")


def _commit_block(
    manifest: Manifest,
    config: RenderConfig,
    snapshot: dict[str, Any],
    audio: np.ndarray,
    index: int,
) -> None:
    block_frames = int(snapshot["block_frames"])
    block = _block_array(audio, index, block_frames)
    if len(block) == 0:
        raise RuntimeError("尝试提交空音频块")
    path = block_path(config.blocks_dir, index)
    payload = np.ascontiguousarray(block).tobytes(order="C")
    atomic_write_bytes(path, payload)
    digest = hashlib.sha256(payload).hexdigest()
    snapshot["blocks"].append(
        {"index": index, "frames": len(block), "sha256": digest}
    )
    snapshot["completed_blocks"] = index + 1
    snapshot["commit_count"] = int(snapshot.get("commit_count", 0)) + 1
    snapshot["status"] = "rendering"
    atomic_write_json(config.snapshot_path, _public_snapshot(snapshot))


def _finalize(
    manifest: Manifest,
    config: RenderConfig,
    alignment: AlignmentResult,
    unmasked: UnmaskResult,
    balance: BalanceResult,
    master: MasterResultBundle,
    snapshot: dict[str, Any],
    warnings_list: list[dict[str, Any]],
    *,
    status: str = "complete",
) -> dict[str, Any]:
    total_frames = int(snapshot["total_frames"])
    _verify_blocks(snapshot, config, manifest, total_frames)
    _write_output_from_blocks(manifest, config, snapshot, total_frames)
    snapshot["status"] = status
    report = _make_report(
        manifest,
        alignment,
        unmasked,
        balance,
        master,
        snapshot=snapshot,
        status=status,
        warnings_list=warnings_list,
        output_path=config.output_path,
    )
    report["master"]["sha256"] = hashlib.sha256(config.output_path.read_bytes()).hexdigest()
    if status == "complete":
        snapshot["final_report"] = report
    atomic_write_json(config.snapshot_path, _public_snapshot(snapshot))
    _write_report(config.report_path, report)
    return report


def _analysis(
    manifest: Manifest,
) -> tuple[dict[str, np.ndarray], dict[str, np.ndarray], AlignmentResult, UnmaskResult, BalanceResult, MasterResultBundle]:
    stems, references = _load_sources(manifest)
    alignment = align_stems(manifest, stems, references)
    unmasked = unmask_stems(manifest, alignment)
    balance = balance_stems(manifest, unmasked, alignment)
    master = _build_master_bundle(manifest, balance, unmasked, alignment)
    return stems, references, alignment, unmasked, balance, master


def _resume_analysis(
    manifest: Manifest,
    snapshot: dict[str, Any],
) -> tuple[dict[str, np.ndarray], dict[str, np.ndarray], AlignmentResult, UnmaskResult, BalanceResult, MasterResultBundle]:
    stems, references = _load_sources(manifest)
    alignment = realign_from_params(manifest, stems, references, snapshot["alignment_params"])
    unmasked = reapply_unmask_params(manifest, alignment, snapshot["unmask_params"])
    balance = _reconstruct_balance(manifest, unmasked, alignment, snapshot["balance_params"])
    master_params = snapshot["master_params"]
    pre_gain = float(master_params["pre_gain"])
    limited = _master_from_pregain(
        balance.raw_stereo,
        manifest.sample_rate,
        pre_gain,
        manifest.true_peak_max_dbtp,
    )
    # Reconstruct a report-compatible object with the same fields produced by the
    # fresh path.
    from types import SimpleNamespace

    report = dict(master_params.get("report", {}))
    master_ns = SimpleNamespace(
        audio=limited.audio,
        pre_gain=pre_gain,
        limiter_gain=limited.gain,
        limiter=limited,
        loudness_before_limiter=float(report.get("loudness_lufs_before_limiter", -120.0)),
        loudness_after_limiter=float(report.get("loudness_lufs_after_limiter", -120.0)),
        true_peak_before_limiter=float(report.get("true_peak_dbtp_before_limiter", -120.0)),
        true_peak_after_limiter=float(report.get("true_peak_dbtp_after_limiter", -120.0)),
        target_loudness_lufs=float(master_params.get("target_loudness_lufs", manifest.target_loudness_lufs)),
        target_true_peak_dbtp=float(master_params.get("true_peak_limit_dbtp", manifest.true_peak_max_dbtp)),
        iterations=int(report.get("loudness_iterations", 1)),
    )
    contributions = {}
    for track in manifest.tracks:
        audio = unmasked.stems[track.role]
        padded = np.zeros(alignment.master_frames, dtype=np.float64)
        padded[: min(len(audio), alignment.master_frames)] = audio[: alignment.master_frames]
        left, right = balance.pan[track.role]
        gain = balance.gains[track.role]
        contributions[track.role] = np.stack(
            (padded * gain * left, padded * gain * right), axis=1
        )
    ratios = _contribution_ratios_after_limiter(contributions, pre_gain, limited.gain)
    ratio_deviation = max(
        (abs(ratios[track.role] - track.target_energy_ratio) for track in manifest.tracks),
        default=0.0,
    )
    reconciled_report = _master_report(
        manifest,
        master_ns,
        ratio_deviation,
    )
    reconciled_report["group_contribution_ratios"] = {
        role: stable_float(value) for role, value in ratios.items()
    }
    reconciled_report["ratio_correction_iterations"] = int(
        report.get("ratio_correction_iterations", 0)
    )
    final_master = MasterResultBundle(
        result=master_ns,
        raw_stereo=balance.raw_stereo,
        track_contributions=contributions,
        contribution_ratios=ratios,
        max_ratio_deviation=ratio_deviation,
        params=master_params,
        report=reconciled_report,
    )
    return stems, references, alignment, unmasked, balance, final_master


def _render_blocks(
    manifest: Manifest,
    config: RenderConfig,
    alignment: AlignmentResult,
    unmasked: UnmaskResult,
    balance: BalanceResult,
    master: MasterResultBundle,
    snapshot: dict[str, Any],
    warnings_list: list[dict[str, Any]],
) -> dict[str, Any]:
    total_blocks = int(snapshot["total_blocks"])
    start = int(snapshot["completed_blocks"])
    audio = master.result.audio
    for index in range(start, total_blocks):
        _commit_block(manifest, config, snapshot, audio, index)
        if config.block_delay_seconds > 0:
            time.sleep(config.block_delay_seconds)
        if config.stop_requested is not None and config.stop_requested():
            snapshot["status"] = "interrupted"
            atomic_write_json(config.snapshot_path, _public_snapshot(snapshot))
            report = _make_report(
                manifest,
                alignment,
                unmasked,
                balance,
                master,
                snapshot=snapshot,
                status="interrupted",
                warnings_list=warnings_list,
                output_path=config.output_path,
            )
            _write_report(config.report_path, report)
            return report
    return _finalize(
        manifest,
        config,
        alignment,
        unmasked,
        balance,
        master,
        snapshot,
        warnings_list,
        status="complete",
    )


def render_project(config: RenderConfig) -> dict[str, Any]:
    manifest = load_manifest(config.manifest_path)
    if config.fresh:
        clear_progress(config.snapshot_path, config.blocks_dir)
    snapshot = _read_snapshot(config.snapshot_path, config.blocks_dir)
    if snapshot is not None and config.block_frames is None:
        block_frames = int(snapshot.get("block_frames", manifest.block_frames))
    else:
        block_frames = int(config.block_frames or manifest.block_frames)
    if block_frames < 256:
        raise BusinessError(
            Issue("E_INVALID_MANIFEST", "block_frames 必须不小于 256", channel="manifest")
        )
    if snapshot is not None:
        try:
            _validate_snapshot_header(snapshot, manifest, config, block_frames)
        except BusinessError:
            raise
        total_frames = int(snapshot.get("total_frames", 0))
        if total_frames <= 0:
            raise _mismatch(config, manifest, "快照缺少有效母带长度")
        _verify_blocks(snapshot, config, manifest, total_frames)
        snapshot["_path"] = str(config.snapshot_path)

    if snapshot is not None and snapshot.get("status") == "complete" and snapshot.get("final_report"):
        _validate_determined_params(manifest, snapshot, config)
        _write_output_from_blocks(
            manifest, config, snapshot, int(snapshot["total_frames"])
        )
        report = dict(snapshot["final_report"])
        report["manifest"] = {"path": str(manifest.path), "sha256": manifest.raw_sha256}
        report["output_path"] = str(config.output_path)
        report["snapshot"]["path"] = str(config.snapshot_path)
        _write_report(config.report_path, report)
        return report

    if snapshot is None:
        stems, references, alignment, unmasked, balance, master = _analysis(manifest)
        total_frames = alignment.master_frames
        total_blocks = int(math.ceil(total_frames / block_frames))
        snapshot = _snapshot_base(manifest, config, block_frames)
        snapshot["total_frames"] = total_frames
        snapshot["total_blocks"] = total_blocks
        snapshot["alignment_params"] = alignment.params
        snapshot["unmask_params"] = unmasked.params
        snapshot["balance_params"] = balance.params
        snapshot["master_params"] = master.params
        snapshot["_path"] = str(config.snapshot_path)
        config.blocks_dir.mkdir(parents=True, exist_ok=True)
        atomic_write_json(config.snapshot_path, _public_snapshot(snapshot))
    else:
        stems, references, alignment, unmasked, balance, master = _resume_analysis(
            manifest, snapshot
        )
        expected_total = alignment.master_frames
        if expected_total != int(snapshot["total_frames"]):
            raise _mismatch(
                config,
                manifest,
                "重建后的母带长度与快照不一致",
                {"expected": expected_total, "snapshot": int(snapshot["total_frames"])},
            )
        # Recompute the deterministic early parameters and reject a snapshot
        # whose committed alignment/unmask state no longer describes this input.
        _validate_determined_params(manifest, snapshot, config)

    warnings_list = list(alignment.warnings)
    for channel in master.report.get("warnings", []) if isinstance(master.report, dict) else []:
        warnings_list.append(channel)
    report = _render_blocks(
        manifest,
        config,
        alignment,
        unmasked,
        balance,
        master,
        snapshot,
        warnings_list,
    )
    return report
