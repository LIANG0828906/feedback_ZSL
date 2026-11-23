"""端到端验证脚本：覆盖任务验收的全部核心链路。

用法（在 audiorest/ 目录、已激活虚拟环境）::

    python -m scripts.run_demo --samples samples --workdir workdir
"""

from __future__ import annotations

import argparse
import json
import shutil
from pathlib import Path

from audiorest.core.engine import RestorationEngine
from audiorest.core.metrics import assert_samples_equal, locate_first_diff, segment_stats
from audiorest.core.pcm import read_pcm
from audiorest.errors import WorkbenchError
from audiorest.formats.loader import (
    assert_equivalent,
    compare_projects,
    export_project,
    load_project,
)
from audiorest.generate_samples import generate


def _ok(message: str) -> None:
    print(f"  [PASS] {message}")


def _expect_error(code: str, fn, *args, **kwargs) -> WorkbenchError:
    try:
        fn(*args, **kwargs)
    except WorkbenchError as exc:
        assert exc.code == code, f"期望 {code}，实际 {exc.code}"
        return exc
    raise AssertionError(f"期望抛出 {code} 但未抛出")


def verify(samples_dir: Path, workdir: Path) -> None:
    workdir.mkdir(parents=True, exist_ok=True)
    manifest = json.loads((samples_dir / "manifest.json").read_text(encoding="utf-8"))

    print("== 1. 多版本加载与跨版本等价性（E_XVER） ==")
    for item in manifest["projects"]:
        project_dir = samples_dir / item["project_id"]
        docs = [project_dir / item["documents"][version] for version in ("1.0", "2.0", "3.0")]
        projects = [load_project(doc) for doc in docs]
        for index, project in enumerate(projects):
            assert project.source_version == ("1.0", "2.0", "3.0")[index]
        for index in range(1, 3):
            report = compare_projects(
                projects[0],
                projects[index],
                left_audio_root=project_dir,
                right_audio_root=project_dir,
            )
            assert report.equivalent, report.first_diff
        assert_equivalent(
            projects[0], projects[2],
            left_audio_root=project_dir, right_audio_root=project_dir,
        )
        _ok(f"{item['project_id']}: v1/v2/v3 内部模型逐项等价，音频哈希一致")

    print("== 2. 升级转换痕迹与降级 E_LOSSY ==")
    first = manifest["projects"][0]
    project_dir = samples_dir / first["project_id"]
    v1_project = load_project(project_dir / first["documents"]["1.0"])
    paths = {entry["field_path"] for entry in v1_project.conversion_trace}
    assert "stages[mask_smoothing].config.smoothing" in paths
    assert "framing.fft_size" in paths
    _ok(f"v1 升级补齐 {len(v1_project.conversion_trace)} 条可追溯痕迹")
    lossy = _expect_error("E_LOSSY", export_project, v1_project, "1.0")
    assert any(loss["category"].startswith("stage") for loss in lossy.details["losses"])
    _ok(f"降级 v1 拒绝静默丢弃，E_LOSSY 列出 {len(lossy.details['losses'])} 项损失")
    export_project(v1_project, "3.0")
    _ok("同级导出 v3 无损失")

    print("== 3. 异常链路：E_VER / E_PARA / E_STATE ==")
    corrupt = samples_dir / "corrupt"
    exc = _expect_error("E_VER", load_project, corrupt / "missing_field.v2.json")
    assert exc.path == "audioRef", exc.path
    _ok(f"必填字段缺失 -> E_VER @ {exc.path}")
    exc = _expect_error("E_VER", load_project, corrupt / "undecodable.json")
    _ok("不可解码 JSON -> E_VER")
    exc = _expect_error("E_VER", load_project, corrupt / "unknown_version.json")
    assert exc.path == "format_version"
    _ok("未知版本号 -> E_VER @ format_version")

    print("== 4. 修复链 + 真值阈值 + 三路径逐样本一致（E_DIFF） ==")
    for item in manifest["projects"]:
        pdir = samples_dir / item["project_id"]
        project = load_project(pdir / item["documents"]["3.0"])
        noisy = read_pcm(pdir / item["noisy_pcm"], project.audio.channels)
        clean = read_pcm(pdir / item["clean_pcm"], project.audio.channels)
        truth = json.loads((pdir / item["truth"]).read_text(encoding="utf-8"))

        ckpt_serial = workdir / item["project_id"] / "serial"
        shutil.rmtree(ckpt_serial, ignore_errors=True)
        serial_engine = RestorationEngine(
            project, noisy, ckpt_serial, pdir / item["noisy_pcm"], max_workers=1
        )
        serial_out = serial_engine.run_incremental()

        ckpt_parallel = workdir / item["project_id"] / "parallel"
        shutil.rmtree(ckpt_parallel, ignore_errors=True)
        parallel_engine = RestorationEngine(
            project, noisy, ckpt_parallel, pdir / item["noisy_pcm"],
            max_workers=3, buffer_capacity_samples=SAMPLE_CAP,
        )
        parallel_out = parallel_engine.run_incremental()
        assert_samples_equal(serial_out, parallel_out, serial_engine.window_starts,
                             "串行 vs 并行")
        _ok(f"{item['project_id']}: 并行与串行逐样本一致")

        # 模拟“实时处理”中途停止：先提交一半窗口
        ckpt_realtime = workdir / item["project_id"] / "realtime"
        shutil.rmtree(ckpt_realtime, ignore_errors=True)
        realtime_engine = RestorationEngine(
            project, noisy, ckpt_realtime, pdir / item["noisy_pcm"], max_workers=1
        )
        half = max(len(realtime_engine.windows) // 2, 1)
        processed = 0

        def stop_after_half(index, progress):
            nonlocal processed
            processed += 1
            if processed >= half:
                realtime_engine.stop()

        partial = realtime_engine.run_incremental(on_window=stop_after_half)
        assert partial.shape[0] > 0
        realtime_engine.stop()
        # 重启新引擎走 resume：有效检查点续跑
        resume_engine = RestorationEngine(
            project, noisy, ckpt_realtime, pdir / item["noisy_pcm"], max_workers=2
        )
        resume_out = resume_engine.resume()
        assert_samples_equal(serial_out, resume_out, serial_engine.window_starts,
                             "实时+断点续跑 vs 全量串行")
        _ok(f"{item['project_id']}: 暂停/退出/重启续跑输出与串行一致")

        # 全量重算路径
        full_out = serial_engine.run_full_recompute()
        assert_samples_equal(serial_out, full_out, serial_engine.window_starts,
                             "全量重算 vs 首次运行")
        _ok(f"{item['project_id']}: 一次性全量重算逐样本一致")

        # 检查点版本不匹配 -> 整体作废重算
        manifest_path = ckpt_serial / "manifest.json"
        doc = json.loads(manifest_path.read_text(encoding="utf-8"))
        doc["algo_version"] = "0.0.1-tampered"
        manifest_path.write_text(json.dumps(doc), encoding="utf-8")
        reloaded = RestorationEngine(
            project, noisy, ckpt_serial, pdir / item["noisy_pcm"], max_workers=1
        )
        assert reloaded._store.load_valid(len(reloaded.windows)) is None
        recomputed = reloaded.run_full_recompute()
        assert_samples_equal(serial_out, recomputed, serial_engine.window_starts,
                             "作废后重算")
        _ok(f"{item['project_id']}: 版本不匹配检查点整体作废，禁止混用旧窗口")

        # 真值阈值逐窗口核对
        for seg in truth["segments"]:
            start, end = seg["start_sample"], seg["end_sample"]
            local_silence = [
                (max(a - start, 0), b - start)
                for a, b in truth["silence_ranges"] if b > start and a < end
            ]
            stats = segment_stats(
                noisy[start:end], serial_out[start:end], clean[start:end],
                local_silence, seg["window_index"],
            )
            assert stats.snr_gain_db >= seg["snr_gain_db_min"], (
                item["project_id"], seg["window_index"], stats
            )
            assert stats.residual_std <= seg["residual_std_max"], (
                item["project_id"], seg["window_index"], stats
            )
        _ok(f"{item['project_id']}: 全部窗口满足 SNR 提升下限与残留标准差上界")

        # 不一致定位能力（人为制造差异，验证 E_DIFF 定位信息）
        tampered = serial_out.copy()
        tampered[min(tampered.shape[0] - 1, 70000), 0] += 0.5
        located = locate_first_diff(serial_out, tampered, serial_engine.window_starts)
        assert located and located["sample_index"] == min(tampered.shape[0] - 1, 70000)
        _ok(f"{item['project_id']}: E_DIFF 定位首个不一致采样 -> 窗口 {located['window_index']}")

    print("== 5. 参数越界 E_PARA 与状态机 E_STATE ==")
    project = load_project(
        samples_dir / first["project_id"] / first["documents"]["3.0"]
    )
    bad = project.with_runtime_params(subtraction_strength=99.0)
    exc = _expect_error("E_PARA", RestorationEngine,
                        bad, read_pcm(samples_dir / first["project_id"] / first["noisy_pcm"],
                                      project.audio.channels),
                        workdir / "para-bad",
                        samples_dir / first["project_id"] / first["noisy_pcm"])
    assert "strength" in exc.path
    _ok(f"谱减强度越界 -> E_PARA @ {exc.path}")

    from audiorest.core.state import WindowState, WindowStateMachine
    machine = WindowStateMachine(2)
    machine.transition(0, WindowState.PROCESSING)
    machine.transition(0, WindowState.COMMITTED)
    exc = _expect_error("E_STATE", machine.transition, 0, WindowState.PROCESSING)
    _ok(f"已提交窗口冻结，非法迁移 -> E_STATE @ {exc.path}")
    assert machine.progress == (1, 2)
    _ok("进度以已提交/总窗口数表示")

    print("\n全部验收项通过。")


SAMPLE_CAP = 48000


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--samples", default="samples")
    parser.add_argument("--workdir", default="workdir")
    parser.add_argument("--regen", action="store_true", help="强制重新生成样本")
    args = parser.parse_args()
    samples_dir = Path(args.samples)
    if args.regen or not (samples_dir / "manifest.json").exists():
        generate(samples_dir)
        print(f"已生成确定性样本: {samples_dir}")
    verify(samples_dir, Path(args.workdir))


if __name__ == "__main__":
    main()
