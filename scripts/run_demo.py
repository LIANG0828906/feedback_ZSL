"""端到端验收脚本。

流程：生成确定性样本库 -> 三版本升级 -> 关节映射 -> 重定向（后台引擎）
-> 一致性校验 -> 快照恢复 -> 降级损失报告 -> 轨迹日志/偏差报告落盘。

用法::

    python scripts/run_demo.py --workspace outputs/demo
"""

from __future__ import annotations

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from retarget_pipeline import compat, engine, gui, mapping, samples, snapshot, trajectory
from retarget_pipeline.codec import dump_file, load_file
from retarget_pipeline.validate import compare_version_runs, validate_retarget

TOLERANCE_RAD = 1e-6


def _write_json(path: str, payload: dict) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, sort_keys=True, indent=2)
        handle.write("\n")


def run(workspace: str, launch_gui: bool = False) -> dict:
    sample_dir = os.path.join(workspace, "samples")
    report_dir = os.path.join(workspace, "reports")
    project_root = os.path.join(workspace, "projects")
    os.makedirs(report_dir, exist_ok=True)

    sets = samples.generate_default_library(sample_dir)
    summary: dict = {"sets": [], "tolerance_rad": TOLERANCE_RAD}
    all_records: list[dict] = []

    worker = engine.RetargetEngine()

    for sample_set in sets:
        set_dir = os.path.join(sample_dir, sample_set.name)
        target = load_file(os.path.join(set_dir, sample_set.target_path))
        version_reports = {}
        version_maps = {}
        version_results = {}

        for version in sample_set.versions:
            raw = load_file(os.path.join(set_dir, sample_set.paths[f"v{version}"]))
            source = compat.upgrade_to_v3(raw)
            joint_map = mapping.build_mapping(source, target)
            version_maps[version] = joint_map

            project_dir = os.path.join(project_root, sample_set.name, f"v{version}")
            handle = worker.submit(
                task_id=f"{sample_set.name}-v{version}",
                source=source,
                target=target,
                mapping=joint_map,
                project_dir=project_dir,
                tolerance_rad=TOLERANCE_RAD,
                animation_key=f"{sample_set.name}:v{version}",
            )
            assert worker.wait(handle, timeout=60), "后台任务未在限时内结束"
            assert handle.state_holder["status"] == "completed", handle.state_holder

            result = handle.state_holder["result"]
            report = handle.state_holder["report"]
            version_results[version] = result
            version_reports[f"v{version}"] = report

            # 落盘重定向目标动画（v3）与偏差报告
            out_doc = _result_to_document(target, result)
            dump_file(out_doc, os.path.join(report_dir,
                       f"{sample_set.name}_retarget_v{version}.json"))
            _write_json(os.path.join(report_dir,
                        f"{sample_set.name}_deviation_v{version}.json"),
                        report.to_dict())
            _write_json(os.path.join(report_dir,
                        f"{sample_set.name}_mapping_v{version}.json"),
                        joint_map.to_dict())

            all_records.extend(trajectory.frame_records(
                sample_set.name, version, joint_map,
                validate_retarget(source, target, joint_map, result, TOLERANCE_RAD),
                trajectory.document_hash(source),
            ))

            # 恢复：仅靠磁盘快照重建并比对最后提交帧
            recovery = snapshot.recover(
                project_dir,
                current_mapping_hash=joint_map.content_hash(),
                current_source_version=source.version,
                current_target_version=target.version,
            )
            assert not recovery.needs_recompute, recovery.recompute_reason
            resolved_count = len(version_results[version].resolved_frames)
            assert len(recovery.frames) == resolved_count
            assert recovery.state.last_committed_frame == resolved_count - 1
            recovered = snapshot.result_from_recovered_frames(
                target.names, recovery.frames, joint_map.content_hash(),
                source.version, target.version,
            )
            assert recovered.frame_count == result.frame_count

        consistency = compare_version_runs(version_reports, TOLERANCE_RAD)

        # 降级损失报告（v3 -> v1，显式接受损失并记录字段清单）
        source_v3 = compat.upgrade_to_v3(
            load_file(os.path.join(set_dir, sample_set.paths["v3"])))
        downgraded, loss = compat.downgrade(source_v3, 1, accept_loss=True)
        dump_file(downgraded, os.path.join(report_dir,
                  f"{sample_set.name}_downgrade_v1.json"))
        _write_json(os.path.join(report_dir,
                    f"{sample_set.name}_version_loss.json"),
                    {"target_version": 1, "lost_fields": loss.lost_fields})

        summary["sets"].append({
            "name": sample_set.name,
            "joint_count": sample_set.joint_count,
            "frame_count": sample_set.frame_count,
            "consistency": consistency,
            "max_angles_deg": consistency["max_angles_deg"],
            "mapping": {
                "hits": version_maps[3].hit_count,
                "total": version_maps[3].source_count,
                "conflicts": len(version_maps[3].conflicts),
                "fallback_targets": len(version_maps[3].fallback_targets),
            },
            "version_loss_fields": sorted(loss.lost_fields),
        })

        if launch_gui:
            gui.launch_gui(target, version_results[3], version_maps[3],
                           version_reports["v3"], offscreen=True)

    log_hash = trajectory.write_trajectory_log(
        os.path.join(report_dir, "trajectory_log.jsonl"), all_records)
    summary["trajectory_log_hash"] = log_hash
    summary["record_count"] = len(all_records)
    _write_json(os.path.join(report_dir, "summary.json"), summary)
    return summary


def _result_to_document(target, result):
    """把重定向结果封装为目标骨架 v3 AnimDocument 以便导出。"""

    import numpy as np

    from retarget_pipeline.model import AnimDocument

    resolved = result.frames
    n = target.joint_count
    rotations = np.stack([f.local_rotations for f in resolved], axis=0)
    translations = np.zeros((len(resolved), n, 3), dtype=np.float64)
    for i, frame in enumerate(resolved):
        translations[i, 0] = frame.root_translation
    return AnimDocument(
        version=3,
        joints=[j.clone() for j in target.joints],
        local_rotations=rotations,
        local_translations=translations,
        frame_rate=target.frame_rate,
        metadata={"mapping_hash": result.mapping_hash},
    )


def main() -> int:
    parser = argparse.ArgumentParser(description="骨骼动画重定向端到端验收")
    parser.add_argument("--workspace", default="outputs/demo")
    parser.add_argument("--gui", action="store_true", help="运行后离屏自检 GUI")
    args = parser.parse_args()
    summary = run(args.workspace, launch_gui=args.gui)
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
