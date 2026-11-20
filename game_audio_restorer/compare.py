"""Cross-version segment comparison with readable attribution."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any, Mapping, Sequence

from .errors import BusinessError, ErrorCode
from .metrics import waveform_delta_db
from .processing import segment_bounds
from .recipes import Recipe
from .restore import RestorationEngine
from .utils import canonical_json, sha256_json
from .wav_io import atomic_write_bytes, read_wav

DEFAULT_COMPARE_TOLERANCES: dict[str, Any] = {
    "common_field_abs": {
        "denoise_strength": 1e-9,
        "gain_db": 1e-9,
        "silence_threshold_dbfs": 1e-9,
    },
    "audio_metric_abs": {
        "residual_noise_dbfs": 1.0,
        "peak_dbfs": 0.5,
        "loudness_lufs": 1.0,
        "gain_jump_db": 1.0,
        "waveform_rms_dbfs": 1.0,
    },
}

_COMMON_FIELDS = ("denoise_strength", "gain_db", "silence_threshold_dbfs")


def _common_fields(recipe: Recipe) -> dict[str, Any]:
    return {
        "denoise_strength": recipe.denoise_strength,
        "gain_db": recipe.gain_db,
        "silence_threshold_dbfs": recipe.silence_threshold_dbfs,
    }


def _delta(left: Any, right: Any) -> float | None:
    if left is None and right is None:
        return 0.0
    if left is None or right is None:
        return None
    return abs(float(left) - float(right))


def _metric_delta(name: str, left: Mapping[str, Any], right: Mapping[str, Any]) -> float:
    if name == "residual_noise_dbfs":
        key = "residual_noise_dbfs"
    else:
        key = name
    return abs(float(left.get(key, -120.0)) - float(right.get(key, -120.0)))


def _recipe_feature_notes(left: Recipe, right: Recipe) -> list[dict[str, str]]:
    notes: list[dict[str, str]] = []
    if left.remove_dc != right.remove_dc:
        notes.append(
            {
                "category": "recipe_version_semantics",
                "field": "remove_dc",
                "message": f"v{left.version} remove_dc={left.remove_dc}，v{right.version} remove_dc={right.remove_dc}；去直流会改变低频残留与峰值。",
            }
        )
    if left.silence_threshold_dbfs != right.silence_threshold_dbfs:
        notes.append(
            {
                "category": "recipe_version_semantics",
                "field": "silence_threshold_dbfs",
                "message": f"静音门限从 {left.silence_threshold_dbfs} 变为 {right.silence_threshold_dbfs} dBFS；静音片段能量会被重新定义。",
            }
        )
    if left.channel_merge_mode != right.channel_merge_mode:
        notes.append(
            {
                "category": "channel_layout",
                "field": "channel_merge_mode",
                "message": f"声道模式从 {left.channel_merge_mode} 变为 {right.channel_merge_mode}；左右波形与通道统计不可直接逐声道对齐。",
            }
        )
    if left.spectral_subtraction != right.spectral_subtraction:
        notes.append(
            {
                "category": "recipe_version_semantics",
                "field": "spectral_subtraction",
                "message": (
                    "谱减参数不同："
                    f"{left.spectral_subtraction.to_dict()} -> "
                    f"{right.spectral_subtraction.to_dict()}；会直接改变降噪残留。"
                ),
            }
        )
    if left.loudness_target_lufs != right.loudness_target_lufs:
        notes.append(
            {
                "category": "recipe_version_semantics",
                "field": "loudness_target_lufs",
                "message": f"响度目标从 {left.loudness_target_lufs} 变为 {right.loudness_target_lufs} LUFS；会改变片段增益与峰谷。",
            }
        )
    return notes


def _metric_notes(
    metric: str,
    left: Recipe,
    right: Recipe,
    metric_notes: list[dict[str, str]],
) -> list[dict[str, str]]:
    notes: list[dict[str, str]] = []
    if metric == "residual_noise_dbfs":
        if left.denoise_strength != right.denoise_strength:
            notes.append(
                {
                    "category": "recipe_version_semantics",
                    "field": "denoise_strength",
                    "message": f"降噪强度不同（{left.denoise_strength} vs {right.denoise_strength}），残留噪声能量超出容差。",
                }
            )
        if left.spectral_subtraction != right.spectral_subtraction:
            notes.append(
                {
                    "category": "recipe_version_semantics",
                    "field": "spectral_subtraction",
                    "message": "v3 分段谱减参数变化是残留噪声差异的首要候选原因。",
                }
            )
        if left.silence_threshold_dbfs != right.silence_threshold_dbfs:
            notes.append(
                {
                    "category": "recipe_version_semantics",
                    "field": "silence_threshold_dbfs",
                    "message": "静音门限差异会改变本片段的残余底噪统计。",
                }
            )
    elif metric in ("peak_dbfs", "gain_jump_db"):
        if left.gain_db != right.gain_db:
            notes.append(
                {
                    "category": "recipe_version_semantics",
                    "field": "gain_db",
                    "message": f"固定增益不同（{left.gain_db} vs {right.gain_db} dB）。",
                }
            )
        if left.loudness_target_lufs != right.loudness_target_lufs:
            notes.append(
                {
                    "category": "recipe_version_semantics",
                    "field": "loudness_target_lufs",
                    "message": "响度归一目标不同，导致应用增益与相邻片段增益跳变不同。",
                }
            )
    elif metric == "loudness_lufs":
        if left.loudness_target_lufs != right.loudness_target_lufs:
            notes.append(
                {
                    "category": "recipe_version_semantics",
                    "field": "loudness_target_lufs",
                    "message": "响度目标变化或一方未启用归一，响度指标超出容差。",
                }
            )
        if left.gain_db != right.gain_db:
            notes.append(
                {
                    "category": "recipe_version_semantics",
                    "field": "gain_db",
                    "message": "固定增益差异会直接影响片段响度。",
                }
            )
    elif metric == "waveform_rms_dbfs":
        if left.channel_merge_mode != right.channel_merge_mode:
            notes.append(
                {
                    "category": "channel_layout",
                    "field": "channel_merge_mode",
                    "message": "声道布局不同，逐声道波形差不可直接归因于采样率。",
                }
            )
    for note in metric_notes:
        if note not in notes:
            notes.append(note)
    if not notes:
        notes.append(
            {
                "category": "undetermined",
                "field": metric,
                "message": "共同字段一致但音频指标超差；请检查处理链数值与输入片段边界。",
            }
        )
    return notes


def compare_versions(
    source_wav: str | os.PathLike[str],
    recipes: Sequence[Recipe],
    work_root: str | os.PathLike[str],
    *,
    tolerances: Mapping[str, Any] | None = None,
    segment_seconds: float = 1.0,
) -> dict[str, Any]:
    if len(recipes) != 3 or sorted(recipe.version for recipe in recipes) != [1, 2, 3]:
        raise BusinessError(
            ErrorCode.RECIPE_INVALID,
            "跨版本比对必须同时提供 v1、v2、v3 各一份配方",
            {"versions": [recipe.version for recipe in recipes]},
        )
    order = sorted(recipes, key=lambda recipe: recipe.version)
    tol = DEFAULT_COMPARE_TOLERANCES if tolerances is None else tolerances
    root = Path(work_root)
    root.mkdir(parents=True, exist_ok=True)
    audio = read_wav(source_wav)
    bounds = segment_bounds(audio.frames, audio.sample_rate, segment_seconds)

    runs: dict[int, Any] = {}
    reports: dict[int, dict[str, Any]] = {}
    output_audio: dict[int, Any] = {}
    for recipe in order:
        version_root = root / f"v{recipe.version}"
        engine = RestorationEngine(
            source_wav,
            recipe,
            version_root,
            output_wav=version_root / "restored.wav",
            report_path=version_root / "report.json",
            trace_path=version_root / "trace.jsonl",
            segment_seconds=segment_seconds,
        )
        run = engine.run()
        if run.status != "completed":
            raise BusinessError(
                ErrorCode.PROCESSING,
                "跨版本比对中的修复任务未完成",
                {"version": recipe.version, "status": run.status},
            )
        runs[recipe.version] = run
        reports[recipe.version] = json.loads(Path(run.report_path).read_text("utf-8"))
        output_audio[recipe.version] = read_wav(run.output_wav)

    recipe_by_version = {recipe.version: recipe for recipe in order}
    pair_versions = ((1, 2), (1, 3), (2, 3))
    segment_rows: list[dict[str, Any]] = []
    summary = {f"v{a}_v{b}": {"incompatible_segments": 0, "compatible_segments": 0} for a, b in pair_versions}

    for index in range(len(bounds)):
        start, end = bounds[index]
        versions_payload: dict[str, Any] = {}
        for version in (1, 2, 3):
            recipe = recipe_by_version[version]
            report = reports[version]
            entry = report["segments"][index]
            output = output_audio[version].samples
            output_channels = output.shape[1]
            versions_payload[f"v{version}"] = {
                "recipe_version": version,
                "recipe_hash": recipe.hash(),
                "common_fields": _common_fields(recipe),
                "field_mapping": list(recipe.field_mapping),
                "sample_rate": audio.sample_rate,
                "input_channels": audio.channels,
                "output_channels": output_channels,
                "metrics": entry["metrics"],
                "content_sha256": entry["content_sha256"],
                "waveform": output[start:end],
            }

        comparisons = []
        for left_version, right_version in pair_versions:
            left = recipe_by_version[left_version]
            right = recipe_by_version[right_version]
            left_payload = versions_payload[f"v{left_version}"]
            right_payload = versions_payload[f"v{right_version}"]
            common_deltas = {
                field: _delta(
                    left_payload["common_fields"][field],
                    right_payload["common_fields"][field],
                )
                for field in _COMMON_FIELDS
            }
            metric_names = (
                "residual_noise_dbfs",
                "peak_dbfs",
                "loudness_lufs",
                "gain_jump_db",
            )
            metric_deltas = {
                name: round(
                    _metric_delta(name, left_payload["metrics"], right_payload["metrics"]),
                    6,
                )
                for name in metric_names
            }
            waveform_delta = waveform_delta_db(
                left_payload["waveform"], right_payload["waveform"]
            )
            metric_deltas["waveform_rms_dbfs"] = round(waveform_delta, 6)

            field_tolerance = tol["common_field_abs"]
            metric_tolerance = tol["audio_metric_abs"]
            field_failures = [
                field
                for field, delta in common_deltas.items()
                if delta is None or delta > float(field_tolerance[field])
            ]
            # A disabled gate on both versions is a representable common value.
            metric_failures = [
                name
                for name, delta in metric_deltas.items()
                if delta > float(metric_tolerance[name])
            ]
            attribution: list[dict[str, str]] = []
            feature_notes = _recipe_feature_notes(left, right)
            for field in field_failures:
                attribution.append(
                    {
                        "category": "common_field",
                        "field": field,
                        "message": (
                            f"共同字段 {field} 不可视为相同："
                            f"v{left_version}={left_payload['common_fields'][field]}，"
                            f"v{right_version}={right_payload['common_fields'][field]}。"
                        ),
                    }
                )
            for name in metric_failures:
                attribution.extend(
                    _metric_notes(name, left, right, feature_notes)
                )
            if left_payload["output_channels"] != right_payload["output_channels"]:
                attribution.append(
                    {
                        "category": "channel_layout",
                        "field": "output_channels",
                        "message": (
                            f"v{left_version} 输出 {left_payload['output_channels']} 声道，"
                            f"v{right_version} 输出 {right_payload['output_channels']} 声道；"
                            "波形差包含布局变化。"
                        ),
                    }
                )
            incompatible = bool(field_failures or metric_failures)
            deduplicated: list[dict[str, str]] = []
            seen_notes: set[tuple[str, str, str]] = set()
            for note in attribution:
                key = (
                    note.get("category", ""),
                    note.get("field", ""),
                    note.get("message", ""),
                )
                if key not in seen_notes:
                    seen_notes.add(key)
                    deduplicated.append(note)
            attribution = deduplicated
            if incompatible and not attribution:
                attribution.append(
                    {
                        "category": "undetermined",
                        "field": "segment",
                        "message": "指标超差但未找到可读字段原因。",
                    }
                )
            pair_key = f"v{left_version}_v{right_version}"
            summary[pair_key][
                "incompatible_segments" if incompatible else "compatible_segments"
            ] += 1
            comparisons.append(
                {
                    "pair": pair_key,
                    "common_field_deltas": common_deltas,
                    "metric_deltas": metric_deltas,
                    "incompatible": incompatible,
                    "attribution": attribution,
                }
            )

        public_versions = {}
        for key, payload in versions_payload.items():
            public_versions[key] = {
                field: value
                for field, value in payload.items()
                if field != "waveform"
            }
        segment_rows.append(
            {
                "segment": index,
                "start_frame": start,
                "end_frame": end,
                "start_s": round(start / audio.sample_rate, 6),
                "end_s": round(end / audio.sample_rate, 6),
                "versions": public_versions,
                "comparisons": comparisons,
            }
        )

    report: dict[str, Any] = {
        "comparison_version": 1,
        "source": {
            "path": audio.source_path,
            "sha256": audio.sha256,
            "sample_rate": audio.sample_rate,
            "channels": audio.channels,
            "frames": audio.frames,
        },
        "sample_rate_note": "三版均以源采样率原生处理，无不必要的重采样；采样率不得作为差异归因。",
        "tolerances": tol,
        "recipes": {
            f"v{recipe.version}": {
                "version": recipe.version,
                "hash": recipe.hash(),
                "normalized": recipe.semantic_dict(),
                "field_mapping": list(recipe.field_mapping),
                "lost_fields": list(recipe.lost_fields),
            }
            for recipe in order
        },
        "runs": {
            f"v{version}": run.to_dict() for version, run in runs.items()
        },
        "segments": segment_rows,
        "summary": summary,
        "segment_count": len(segment_rows),
        "hash": "",
    }
    hash_payload = {
        key: value for key, value in report.items() if key != "hash"
    }
    hash_payload["source"] = {
        key: value
        for key, value in hash_payload["source"].items()
        if key != "path"
    }
    hash_payload["runs"] = {
        version: {
            key: value
            for key, value in run.items()
            if key
            not in (
                "snapshot_dir",
                "output_wav",
                "report_path",
                "trace_path",
            )
        }
        for version, run in hash_payload["runs"].items()
    }
    report["hash"] = sha256_json(hash_payload)
    return report


def comparison_trace(report: Mapping[str, Any]) -> bytes:
    lines: list[bytes] = []
    header = {
        "trace_version": 1,
        "source_sha256": report["source"]["sha256"],
        "tolerances": report["tolerances"],
        "comparison_hash": report["hash"],
    }
    lines.append((canonical_json(header) + "\n").encode("utf-8"))
    for row in report["segments"]:
        trace_row = {
            "segment": row["segment"],
            "start_frame": row["start_frame"],
            "end_frame": row["end_frame"],
            "versions": {
                key: {
                    "recipe_version": value["recipe_version"],
                    "field_mapping": value["field_mapping"],
                    "metrics": value["metrics"],
                    "content_sha256": value["content_sha256"],
                }
                for key, value in row["versions"].items()
            },
            "comparisons": row["comparisons"],
        }
        lines.append((canonical_json(trace_row) + "\n").encode("utf-8"))
    return b"".join(lines)


def write_compare_outputs(
    report: Mapping[str, Any],
    report_path: str | os.PathLike[str],
    trace_path: str | os.PathLike[str],
) -> None:
    report_bytes = (
        json.dumps(
            report,
            ensure_ascii=False,
            sort_keys=True,
            indent=2,
            allow_nan=False,
        ).encode("utf-8")
        + b"\n"
    )
    atomic_write_bytes(report_path, report_bytes)
    atomic_write_bytes(trace_path, comparison_trace(report))
