"""Automated acceptance checks for three run paths and error codes."""

from __future__ import annotations

import json
import os
import platform
import shutil
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, Mapping, Optional, Sequence

import cv2
import numpy as np

from .checkpoint import CheckpointStore, SnapshotValidation
from .config import ALGORITHM_VERSION, STAGES, PipelineConfig
from .diff import (
    compare_commit_sequences,
    compare_geometry,
    compare_stage_masks,
    difference_heatmap,
    raise_for_mask_difference,
)
from .engine import (
    PipelineEngine,
    PipelineResult,
    SimulatedCrash,
    canonical_input_hash,
)
from .errors import EdgePipeError, ErrorCode
from .ops import BlockGrid, run_whole_pipeline
from .samples import (
    decode_image,
    find_sample,
    generate_samples,
    load_manifest,
    sample_image_path,
    truth_from_sample,
)
from .states import COMMITTED, PENDING, BlockStageStateMachine


def build_sample_config(
    sample: Mapping[str, Any], overrides: Optional[Mapping[str, Any]] = None
) -> PipelineConfig:
    values = dict(sample.get("recommended_config", {}))
    if overrides:
        values.update(overrides)
    return PipelineConfig.from_mapping(values)


def verify_three_paths(
    sample: Mapping[str, Any],
    manifest_dir: str | Path,
    work_root: str | Path,
    *,
    prefix_commits: int = 7,
    config_overrides: Optional[Mapping[str, Any]] = None,
    save_masks: bool = True,
) -> Dict[str, Any]:
    config = build_sample_config(sample, config_overrides)
    image = decode_image(sample_image_path(sample, manifest_dir))
    config.validate_image(image)
    truth = truth_from_sample(sample)
    grid = BlockGrid.create(image.shape[0], image.shape[1], config.block_size)
    total_commits = len(grid.blocks) * len(STAGES)
    prefix = max(1, min(prefix_commits, total_commits - 1))
    sample_work = Path(work_root) / str(sample["name"])
    sample_work.mkdir(parents=True, exist_ok=True)

    full_root = sample_work / "full-checkpoints"
    incremental_root = sample_work / "incremental-checkpoints"
    resume_root = sample_work / "resume-checkpoints"
    for root in (full_root, incremental_root, resume_root):
        shutil.rmtree(root, ignore_errors=True)

    full = PipelineEngine(config, full_root).run(
        image, truth=truth, mode="full"
    )
    if not full.complete:
        raise EdgePipeError(
            ErrorCode.INTERNAL,
            "全量路径未完成",
            details={"failed_tasks": full.failed_tasks},
        )

    incremental_initial = PipelineEngine(config, incremental_root).run(
        image,
        truth=truth,
        mode="full",
        stop_after_commits=prefix,
    )
    incremental = PipelineEngine(config, incremental_root).run(
        image, truth=truth, mode="incremental"
    )

    resume_initial_error = None
    try:
        PipelineEngine(config, resume_root).run(
            image,
            truth=truth,
            mode="full",
            crash_after_commits=prefix,
        )
    except SimulatedCrash as exc:
        resume_initial_error = str(exc)
    resumed = PipelineEngine(config, resume_root).run(
        image, truth=truth, mode="resume"
    )

    if not incremental.complete or not resumed.complete:
        raise EdgePipeError(
            ErrorCode.INTERNAL,
            "增量或恢复路径未完成",
            details={
                "incremental_complete": incremental.complete,
                "resume_complete": resumed.complete,
                "incremental_failed": incremental.failed_tasks,
                "resume_failed": resumed.failed_tasks,
            },
        )

    raise_for_mask_difference(
        full.final_mask,
        incremental.final_mask,
        stage="final",
        grid=grid,
    )
    raise_for_mask_difference(
        full.final_mask,
        resumed.final_mask,
        stage="final",
        grid=grid,
    )
    compare_stage_masks(full.stage_masks, incremental.stage_masks, grid=grid)
    compare_stage_masks(full.stage_masks, resumed.stage_masks, grid=grid)
    compare_geometry(full.geometry, incremental.geometry)
    compare_geometry(full.geometry, resumed.geometry)
    compare_commit_sequences(full.commit_sequence, incremental.commit_sequence)
    compare_commit_sequences(full.commit_sequence, resumed.commit_sequence)
    if full.states != incremental.states or full.states != resumed.states:
        raise EdgePipeError(
            ErrorCode.DIFF,
            "分块阶段状态矩阵不一致",
            field="states",
            details={"coordinate": None, "block_id": None, "stage": "states"},
        )

    whole_mask, whole_stages, whole_geometry = PipelineEngine.whole_reference(
        image, config, truth
    )
    compare_stage_masks(full.stage_masks, whole_stages, grid=grid)
    raise_for_mask_difference(
        full.final_mask, whole_mask, stage="final", grid=grid
    )
    compare_geometry(full.geometry, whole_geometry)
    boundary_max_diff = int(
        np.count_nonzero(full.final_mask != whole_mask)
    )

    invalidation = verify_checkpoint_invalidation(
        full_root=full_root,
        image=image,
        config=config,
        grid=grid,
    )

    mask_outputs: Dict[str, str] = {}
    if save_masks:
        mask_dir = Path(work_root) / "masks"
        mask_dir.mkdir(parents=True, exist_ok=True)
        full_path = mask_dir / f"{sample['name']}_full.png"
        if not cv2.imwrite(str(full_path), full.final_mask):
            raise EdgePipeError(ErrorCode.IO, "无法写入最终掩膜", field="path")
        mask_outputs["full"] = str(full_path)
        heat = difference_heatmap(full.final_mask, whole_mask)
        heat_path = mask_dir / f"{sample['name']}_full_vs_whole_heatmap.png"
        if not cv2.imwrite(str(heat_path), heat):
            raise EdgePipeError(ErrorCode.IO, "无法写入差异热力图", field="path")
        mask_outputs["full_vs_whole_heatmap"] = str(heat_path)

    return {
        "name": sample["name"],
        "image_shape": list(image.shape),
        "config": config.to_dict(),
        "params_version": config.params_version,
        "input_hash": canonical_input_hash(image),
        "prefix_commits": prefix,
        "total_commits": total_commits,
        "incremental_initial_complete": incremental_initial.complete,
        "incremental_initial_commits": incremental_initial.progress["committed"],
        "resume_initial_error": resume_initial_error,
        "paths": {
            "full": {
                "complete": full.complete,
                "restored_commits": full.restored_commits,
                "newly_committed": full.newly_committed,
                "elapsed_seconds": round(full.elapsed_seconds, 6),
            },
            "incremental": {
                "complete": incremental.complete,
                "restored_commits": incremental.restored_commits,
                "newly_committed": incremental.newly_committed,
                "elapsed_seconds": round(incremental.elapsed_seconds, 6),
            },
            "resume": {
                "complete": resumed.complete,
                "restored_commits": resumed.restored_commits,
                "newly_committed": resumed.newly_committed,
                "elapsed_seconds": round(resumed.elapsed_seconds, 6),
            },
        },
        "three_paths_masks_equal": True,
        "three_paths_stages_equal": True,
        "three_paths_geometry_equal": True,
        "three_paths_commit_sequence_equal": True,
        "three_paths_state_matrix_equal": True,
        "chunk_vs_whole_stages_equal": True,
        "chunk_vs_whole_final_equal": True,
        "chunk_vs_whole_max_pixel_difference": boundary_max_diff,
        "effective_band_width": config.effective_band_width,
        "required_band_width": config.required_band_width,
        "geometry": full.geometry,
        "geometry_all_pass": bool(
            full.geometry is not None and full.geometry["all_pass"]
        ),
        "checkpoint_invalidation": invalidation,
        "checkpoint_size_bytes": PipelineEngine(config, full_root).store.snapshot_size_bytes(),
        "masks": mask_outputs,
        "all_pass": True,
    }


def verify_checkpoint_invalidation(
    *,
    full_root: str | Path,
    image: np.ndarray,
    config: PipelineConfig,
    grid: BlockGrid,
) -> Dict[str, Any]:
    source = Path(full_root)
    copy_root = source.parent / "invalidation-copy"
    shutil.rmtree(copy_root, ignore_errors=True)
    shutil.copytree(source, copy_root)
    store = CheckpointStore(copy_root)
    image_hash = canonical_input_hash(image)
    healthy = store.validate_snapshot(
        expected_input_hash=image_hash,
        expected_params_version=config.params_version,
        expected_algorithm_version=ALGORITHM_VERSION,
        grid=grid,
    )
    params_mismatch = store.validate_snapshot(
        expected_input_hash=image_hash,
        expected_params_version="forced-parameter-mismatch",
        expected_algorithm_version=ALGORITHM_VERSION,
        grid=grid,
    )
    algorithm_mismatch = store.validate_snapshot(
        expected_input_hash=image_hash,
        expected_params_version=config.params_version,
        expected_algorithm_version="forced-algorithm-mismatch",
        grid=grid,
    )
    input_mismatch = store.validate_snapshot(
        expected_input_hash="forced-input-mismatch",
        expected_params_version=config.params_version,
        expected_algorithm_version=ALGORITHM_VERSION,
        grid=grid,
    )

    manifest = json.loads((copy_root / "snapshot.json").read_text(encoding="utf-8"))
    target = manifest["entries"][len(manifest["entries"]) // 2]
    blob_path = copy_root / target["path"]
    with np.load(str(blob_path), allow_pickle=False) as archive:
        mask = np.array(archive["mask"], copy=True)
        meta = np.array(archive["meta"], copy=True)
    flat = mask.reshape(-1)
    flat[0] = 1 if flat[0] == 0 else 0
    with open(blob_path, "wb") as handle:
        np.savez_compressed(handle, mask=mask, meta=meta)
    content_mismatch = store.validate_snapshot(
        expected_input_hash=image_hash,
        expected_params_version=config.params_version,
        expected_algorithm_version=ALGORITHM_VERSION,
        grid=grid,
    )

    result = {
        "healthy_status": healthy.status,
        "params_mismatch": _validation_record(params_mismatch),
        "algorithm_mismatch": _validation_record(algorithm_mismatch),
        "input_mismatch": _validation_record(input_mismatch),
        "content_mismatch": _validation_record(content_mismatch),
    }
    result["all_invalidations_detected"] = bool(
        healthy.valid
        and not params_mismatch.valid
        and "参数版本" in (params_mismatch.reason or "")
        and not algorithm_mismatch.valid
        and "算法版本" in (algorithm_mismatch.reason or "")
        and not input_mismatch.valid
        and "输入内容哈希" in (input_mismatch.reason or "")
        and not content_mismatch.valid
        and "内容哈希" in (content_mismatch.reason or "")
    )
    if not result["all_invalidations_detected"]:
        raise EdgePipeError(
            ErrorCode.INTERNAL,
            "检查点失效检测未达到预期",
            details=result,
        )
    shutil.rmtree(copy_root, ignore_errors=True)
    return result


def _validation_record(validation: SnapshotValidation) -> Dict[str, Any]:
    return {
        "status": validation.status,
        "valid": validation.valid,
        "reason": validation.reason,
    }


def verify_error_codes(
    *,
    image: np.ndarray,
    config: PipelineConfig,
    full_result: PipelineResult,
    temp_dir: str | Path,
) -> Dict[str, Any]:
    checks: Dict[str, Any] = {}

    try:
        PipelineConfig(low_threshold=42.0, high_threshold=11.0)
    except EdgePipeError as exc:
        checks["E_PARA_threshold_order"] = _error_record(exc)
        if exc.code != ErrorCode.PARA or exc.field != "low_threshold":
            raise
    try:
        PipelineConfig(high_threshold=99999.0)
    except EdgePipeError as exc:
        checks["E_PARA_threshold_range"] = _error_record(exc)
        if exc.code != ErrorCode.PARA or exc.field != "high_threshold":
            raise
    try:
        PipelineConfig(band_width=1)
    except EdgePipeError as exc:
        checks["E_BAND_insufficient"] = _error_record(exc)
        if exc.code != ErrorCode.BAND or exc.field != "band_width":
            raise

    machine = BlockStageStateMachine(1)
    try:
        machine.transition("b0000", "gradient", COMMITTED)
    except EdgePipeError as exc:
        checks["E_STATE_illegal_transition"] = _error_record(exc)
        if exc.code != ErrorCode.STATE:
            raise
        if machine.get("b0000", "gradient").state != PENDING:
            raise EdgePipeError(
                ErrorCode.INTERNAL, "非法迁移改变了原状态", field="state"
            )

    broken = Path(temp_dir) / "broken.png"
    broken.parent.mkdir(parents=True, exist_ok=True)
    broken.write_bytes(b"not-a-valid-image")
    try:
        decode_image(broken)
    except EdgePipeError as exc:
        checks["E_DECODE_corrupt_image"] = _error_record(exc)
        if exc.code != ErrorCode.DECODE:
            raise

    perturbed = full_result.final_mask.copy()
    location = (image.shape[0] // 2, image.shape[1] // 2)
    if perturbed.ndim == 3:
        perturbed[location[0], location[1], 0] ^= 255
    else:
        perturbed[location[0], location[1]] ^= 255
    grid = BlockGrid.create(image.shape[0], image.shape[1], config.block_size)
    try:
        raise_for_mask_difference(
            full_result.final_mask, perturbed, stage="final", grid=grid
        )
    except EdgePipeError as exc:
        checks["E_DIFF_first_pixel"] = _error_record(exc)
        if exc.code != ErrorCode.DIFF:
            raise
        expected_block = grid.block_at(*location).block_id
        coordinate = exc.details.get("coordinate", {})
        if (
            coordinate.get("y") != location[0]
            or coordinate.get("x") != location[1]
            or exc.details.get("block_id") != expected_block
            or exc.details.get("stage") != "final"
        ):
            raise EdgePipeError(
                ErrorCode.INTERNAL,
                "E_DIFF 未正确定位首个差异",
                details=exc.details,
            )

    return checks


def _error_record(exc: EdgePipeError) -> Dict[str, Any]:
    return exc.to_dict()


def verify_all(
    *,
    manifest_path: str | Path = "samples/index.json",
    work_dir: str | Path = "artifacts/validation_work",
    report_path: str | Path = "reports/validation_report.json",
    markdown_path: str | Path = "docs/VALIDATION.md",
    regenerate_samples: bool = False,
) -> Dict[str, Any]:
    manifest_path = Path(manifest_path)
    if regenerate_samples or not manifest_path.exists():
        generate_samples(manifest_path.parent, clean=False)
    manifest = load_manifest(manifest_path)
    work_root = Path(work_dir)
    shutil.rmtree(work_root, ignore_errors=True)
    work_root.mkdir(parents=True, exist_ok=True)

    sample_reports: List[Dict[str, Any]] = []
    for sample in manifest["samples"]:
        try:
            sample_reports.append(
                verify_three_paths(
                    sample,
                    manifest_path.parent,
                    work_root,
                )
            )
        except EdgePipeError as exc:
            sample_reports.append(
                {
                    "name": sample.get("name"),
                    "all_pass": False,
                    "error": exc.to_dict(),
                }
            )

    first_sample = manifest["samples"][0]
    first_image = decode_image(sample_image_path(first_sample, manifest_path.parent))
    first_config = build_sample_config(first_sample)
    first_full_root = work_root / first_sample["name"] / "full-checkpoints"
    first_full = PipelineEngine(first_config, first_full_root).run(
        first_image,
        truth=truth_from_sample(first_sample),
        mode="incremental",
    )
    error_checks = verify_error_codes(
        image=first_image,
        config=first_config,
        full_result=first_full,
        temp_dir=work_root / "error-code-temp",
    )

    report = {
        "schema": "edgepipe.validation/v1",
        "generated_at_utc": datetime.now(timezone.utc).isoformat(),
        "environment": {
            "python": sys.version,
            "platform": platform.platform(),
            "numpy": np.__version__,
            "opencv": cv2.__version__,
            "algorithm_version": ALGORITHM_VERSION,
        },
        "samples": sample_reports,
        "error_codes": error_checks,
        "all_pass": all(item.get("all_pass", False) for item in sample_reports),
    }

    report_file = Path(report_path)
    report_file.parent.mkdir(parents=True, exist_ok=True)
    report_file.write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True),
        encoding="utf-8",
    )
    markdown_file = Path(markdown_path)
    markdown_file.parent.mkdir(parents=True, exist_ok=True)
    markdown_file.write_text(_render_markdown(report), encoding="utf-8")
    return report


def _render_markdown(report: Mapping[str, Any]) -> str:
    lines = [
        "# 实际验证记录",
        "",
        f"- 生成时间（UTC）：`{report['generated_at_utc']}`",
        f"- 算法版本：`{report['environment']['algorithm_version']}`",
        f"- Python：`{report['environment']['python'].splitlines()[0]}`",
        f"- NumPy/OpenCV：`{report['environment']['numpy']}` / `{report['environment']['opencv']}`",
        f"- 总体结果：**{'通过' if report['all_pass'] else '失败'}**",
        "",
        "## 样本验收矩阵",
        "",
        "| 样本 | 三路径逐像素 | 分块/整图阶段一致 | 提交序列一致 | 几何真值 | 检查点失效 | 分块×阶段 | 有效边带 |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for item in report["samples"]:
        if "error" in item:
            lines.append(
                f"| `{item['name']}` | 失败 | - | - | - | - | - | - |"
            )
            continue
        geom = item["geometry"]
        lines.append(
            "| `{name}` | {three} | {boundary} | {seq} | {geom} | {invalidate} | {total} | {band}px |".format(
                name=item["name"],
                three="是" if item["three_paths_masks_equal"] else "否",
                boundary="是" if item["chunk_vs_whole_stages_equal"] else "否",
                seq="是" if item["three_paths_commit_sequence_equal"] else "否",
                geom=(
                    f"通过（最大位移 {geom['max_abs_systematic_displacement_px']:.3f}px）"
                    if item["geometry_all_pass"]
                    else "未通过"
                ),
                invalidate=(
                    "是"
                    if item["checkpoint_invalidation"]["all_invalidations_detected"]
                    else "否"
                ),
                total=item["total_commits"],
                band=item["effective_band_width"],
            )
        )

    lines.extend(
        [
            "",
            "## 三路径运行摘要",
            "",
            "| 样本 | 路径 | 复用提交 | 新提交 | 耗时(s) |",
            "|---|---|---:|---:|---:|",
        ]
    )
    for item in report["samples"]:
        if "error" in item:
            continue
        for mode in ("full", "incremental", "resume"):
            path = item["paths"][mode]
            lines.append(
                f"| `{item['name']}` | `{mode}` | {path['restored_commits']} | "
                f"{path['newly_committed']} | {path['elapsed_seconds']:.6f} |"
            )

    lines.extend(
        [
            "",
            "## 几何真值统计",
            "",
            "| 样本 | 掩膜像素 | 最大系统位移(px) | 最大圆半径误差(px) | 覆盖/位移/圆拟合 |",
            "|---|---:|---:|---:|---|",
        ]
    )
    for item in report["samples"]:
        if "error" in item:
            continue
        geom = item["geometry"]
        lines.append(
            f"| `{item['name']}` | {geom['mask_pixels']} | "
            f"{geom['max_abs_systematic_displacement_px']:.6f} | "
            f"{geom['max_circle_radius_error_px']:.6f} | "
            f"{geom['coverage_ok']}/{geom['displacement_ok']}/{geom['circle_fit_ok']} |"
        )

    lines.extend(
        [
            "",
            "## 异常码实测",
            "",
            "| 检查 | 错误码 | 字段 | 说明 |",
            "|---|---|---|---|",
        ]
    )
    for name, record in report["error_codes"].items():
        lines.append(
            f"| `{name}` | `{record['code']}` | `{record.get('field')}` | "
            f"{record['message']} |"
        )

    lines.extend(
        [
            "",
            "## 说明",
            "",
            "- 三路径比较覆盖五个阶段掩膜、最终掩膜、几何统计、提交序列与状态矩阵。",
            "- 断点恢复路径在写入指定数量原子提交后抛出模拟崩溃，再由新引擎实例恢复。",
            "- 检查点验收复制完整快照后分别注入参数版本、算法版本、输入哈希和分块内容哈希失配；四种情形均被整体判废。",
            "- 分块边界验收将每阶段分块合并结果与整图单块结果逐像素比较，未裁掉任何边界像素。",
        ]
    )
    return "\n".join(lines) + "\n"
