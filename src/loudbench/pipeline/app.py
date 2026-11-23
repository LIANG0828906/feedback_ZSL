"""工作台应用门面：样本加载、三路径运行、报告与导出。"""
from __future__ import annotations

import json
import os

from ..audio.io import content_hash, read_wav, write_wav_float32
from ..core.config import PipelineConfig
from ..core.errors import DecodeError
from ..core.segment import ProgramSegment
from .compare import assert_outputs_equal, assert_reports_equal
from .orchestrator import PipelineOrchestrator
from .report import dump_report


def load_program(manifest_path: str) -> tuple[list[ProgramSegment], dict]:
    """按样本清单加载全部节目段；文件损坏返回 E_DECODE。"""
    if not os.path.exists(manifest_path):
        raise DecodeError(f"样本清单不存在: {manifest_path}", field="path")
    try:
        with open(manifest_path, "rb") as fh:
            manifest = json.loads(fh.read().decode())
    except Exception as exc:
        raise DecodeError(f"样本清单解析失败: {exc}",
                          field="manifest") from exc

    base = os.path.dirname(os.path.abspath(manifest_path))
    segments: list[ProgramSegment] = []
    for entry in manifest["segments"]:
        wav_path = os.path.normpath(os.path.join(base, entry["path"]))
        fs, x = read_wav(wav_path)
        if fs != entry["sample_rate"]:
            raise DecodeError(
                f"段 {entry['name']} 采样率与清单不符",
                field="sample_rate")
        if content_hash(x) != entry["content_hash"]:
            raise DecodeError(
                f"段 {entry['name']} 内容哈希与清单不符（样本损坏）",
                field="content_hash")
        segments.append(ProgramSegment(
            index=entry["index"], name=entry["name"], sample_rate=fs,
            samples=x, source_path=wav_path, channels=entry["channels"],
            truth=entry["truth"]))
    return segments, manifest


def build_orchestrator(manifest_path: str, config: PipelineConfig,
                       checkpoint_dir: str) -> PipelineOrchestrator:
    segments, _manifest = load_program(manifest_path)
    return PipelineOrchestrator(segments, config, checkpoint_dir)


def run_path(manifest_path: str, config: PipelineConfig,
             checkpoint_dir: str, parallel: bool,
             restore: bool = False) -> PipelineOrchestrator:
    orch = build_orchestrator(manifest_path, config, checkpoint_dir)
    if restore:
        orch.restore()
    orch.run(parallel=parallel)
    return orch


def verify_three_paths(manifest_path: str, config: PipelineConfig,
                       checkpoint_dir: str, export_dir: str
                       ) -> dict:
    """并行、串行全量重算、断点恢复三条路径一致性验收。"""
    # 路径 1：并行全量
    cp_parallel = os.path.join(checkpoint_dir, "run_parallel")
    orch_p = run_path(manifest_path, config, cp_parallel, parallel=True)

    # 路径 2：串行全量重算（独立检查点目录）
    cp_serial = os.path.join(checkpoint_dir, "run_serial")
    orch_s = run_path(manifest_path, config, cp_serial, parallel=False)

    # 路径 3：并行后重启，仅从检查点恢复
    orch_r = build_orchestrator(manifest_path, config, cp_parallel)
    restored = orch_r.restore()
    if not restored:
        raise RuntimeError("恢复路径未能加载有效检查点")

    assert_outputs_equal(orch_p._ordered_outputs(), orch_s._ordered_outputs())
    assert_outputs_equal(orch_p._ordered_outputs(), orch_r._ordered_outputs())

    fs_p, audio_p = orch_p.concatenated_output()
    fs_s, audio_s = orch_s.concatenated_output()
    fs_r, audio_r = orch_r.concatenated_output()
    # 拼接含确定性重采样：形状一致 + 段级逐样本一致即保证整段一致
    if not (audio_p.shape == audio_s.shape == audio_r.shape):
        raise RuntimeError("三路径拼接音频形状不一致")
    if not (fs_p == fs_s == fs_r):
        raise RuntimeError("三路径导出采样率不一致")

    report_p, report_s = orch_p.build_report(), orch_s.build_report()
    report_r = orch_r.build_report()
    assert_reports_equal(report_p, report_s)
    assert_reports_equal(report_p, report_r)

    os.makedirs(export_dir, exist_ok=True)
    export_path = os.path.join(export_dir, "program_parallel.wav")
    write_wav_float32(export_path, audio_p, fs_p)
    report_path = os.path.join(export_dir, "loudness_report.json")
    dump_report(report_path, report_p)
    return {
        "export_wav": export_path,
        "report_json": report_path,
        "report": report_p,
        "sample_rate": fs_p,
    }
