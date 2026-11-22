#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import re
import shutil
import signal
import struct
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / "src"
sys.path.insert(0, str(SRC))

import numpy as np  # noqa: E402

from live_transcode.engine import TranscodeEngine  # noqa: E402
from live_transcode.errors import TaskError  # noqa: E402
from live_transcode.models import canonical_json_bytes  # noqa: E402
from live_transcode.samples import generate_fixtures  # noqa: E402
from live_transcode.sidecar import estimation_contract  # noqa: E402
from live_transcode.wavio import inspect_wav  # noqa: E402


def rel(path: Path) -> str:
    try:
        return str(path.resolve().relative_to(ROOT))
    except ValueError:
        return str(path)


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )


def trace_records(path: Path) -> list[dict[str, Any]]:
    return [
        json.loads(line)
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def unittest_demo(acceptance: Path) -> dict[str, Any]:
    environment = dict(os.environ)
    environment["QT_QPA_PLATFORM"] = "offscreen"
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "unittest",
            "discover",
            "-s",
            "tests",
            "-t",
            ".",
            "-v",
        ],
        cwd=str(ROOT),
        env=environment,
        capture_output=True,
        text=True,
        timeout=180,
    )
    output = completed.stdout + completed.stderr
    log_path = acceptance / "unittest.log"
    log_path.write_text(output, encoding="utf-8")
    count_match = re.search(r"Ran (\d+) tests? in ", output)
    return {
        "ok": completed.returncode == 0 and "\nOK" in output,
        "returncode": completed.returncode,
        "tests_run": int(count_match.group(1)) if count_match else 0,
        "log": rel(log_path),
    }


def gui_smoke_demo(acceptance: Path, wav: Path) -> dict[str, Any]:
    os.environ["QT_QPA_PLATFORM"] = "offscreen"
    from PySide6.QtWidgets import QApplication

    from live_transcode.gui import ReviewWindow

    app = QApplication.instance() or QApplication([])
    store = acceptance / "gui_store"
    if store.exists():
        shutil.rmtree(store)
    window = ReviewWindow(str(store), str(acceptance / "samples"))
    window.wav_path.setText(str(wav))
    window._load_current()
    panel = window.panels["review44"]
    panel.rate.setValue(24000)
    panel.filter_length.setValue(32)
    panel.quant.setCurrentText("24")
    panel.slice_samples.setValue(256)
    panel.start()
    deadline = time.time() + 60
    while (
        panel.worker is not None
        and panel.worker.isRunning()
        and time.time() < deadline
    ):
        app.processEvents()
        time.sleep(0.005)
    app.processEvents()
    generated = panel.last_version_hash is not None

    panel.audit()
    deadline = time.time() + 60
    while (
        panel.audit_worker is not None
        and panel.audit_worker.isRunning()
        and time.time() < deadline
    ):
        app.processEvents()
        time.sleep(0.005)
    app.processEvents()
    audit_passed = "Audit passed" in panel.status.text()

    screenshot = acceptance / "gui_smoke.png"
    window.show()
    app.processEvents()
    saved = window.grab().save(str(screenshot), "PNG")
    trace_path = store / "trace.jsonl"
    result = {
        "ok": generated and audit_passed and saved,
        "generated": generated,
        "audit_passed": audit_passed,
        "screenshot": rel(screenshot),
        "screenshot_saved": saved,
        "status": panel.status.text(),
        "tier_version_hash": panel.last_version_hash,
        "trace": rel(trace_path),
    }
    window.close()
    app.processEvents()
    return result


def find_run_dir(store: Path, tier_id: str) -> Path:
    snapshots = sorted(store.glob(f"runs/{tier_id}/*/snapshot.json"))
    if not snapshots:
        raise RuntimeError(f"no snapshot for {tier_id}")
    return snapshots[-1].parent


def wav_invalid_demo(source: Path, output: Path) -> dict[str, Any]:
    raw = bytearray(source.read_bytes())
    offset = 12
    while offset + 8 <= len(raw):
        chunk_id = raw[offset : offset + 4]
        size = struct.unpack_from("<I", raw, offset + 4)[0]
        if chunk_id == b"fmt ":
            struct.pack_into("<I", raw, offset + 8 + 20, 0x3F)
            break
        offset += 8 + size + (size & 1)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_bytes(raw)
    try:
        inspect_wav(output)
    except TaskError as exc:
        return exc.as_dict()
    return {"code": "E_EXPECTED_FAILURE_NOT_RAISED"}


def drift_cases(
    engine: TranscodeEngine,
    base_result,
    changed_result,
    acceptance: Path,
) -> dict[str, Any]:
    cases_root = acceptance / "drift_cases"
    if cases_root.exists():
        shutil.rmtree(cases_root)
    cases_root.mkdir(parents=True)

    sidecar_case = cases_root / "sidecar_stale"
    shutil.copytree(changed_result.run_dir, sidecar_case / "run")
    sidecar_path = sidecar_case / "run" / "sidecar.json"
    sidecar = json.loads(sidecar_path.read_text(encoding="utf-8"))
    old_sidecar = json.loads(
        (base_result.run_dir / "sidecar.json").read_text(encoding="utf-8")
    )
    drift_index = 20
    sidecar["slices"][drift_index] = old_sidecar["slices"][drift_index]
    sidecar_path.write_bytes(canonical_json_bytes(sidecar) + b"\n")
    try:
        engine.audit_committed_run(
            str((acceptance / "samples" / "mono_24bit_seed19093.wav")),
            sidecar_case / "run",
        )
        sidecar_result: dict[str, Any] = {"code": "E_EXPECTED_FAILURE_NOT_RAISED"}
    except TaskError as exc:
        sidecar_result = exc.as_dict()

    buffer_case = cases_root / "buffer_stale"
    shutil.copytree(changed_result.run_dir, buffer_case / "run")
    shutil.copyfile(
        base_result.run_dir / "slices" / f"{drift_index:06d}" / "audio.pcm",
        buffer_case / "run" / "slices" / f"{drift_index:06d}" / "audio.pcm",
    )
    try:
        engine.audit_committed_run(
            str((acceptance / "samples" / "mono_24bit_seed19093.wav")),
            buffer_case / "run",
        )
        buffer_result: dict[str, Any] = {"code": "E_EXPECTED_FAILURE_NOT_RAISED"}
    except TaskError as exc:
        buffer_result = exc.as_dict()
    return {
        "sidecar_stale": sidecar_result,
        "buffer_stale": buffer_result,
        "drift_index": drift_index,
        "case_root": rel(cases_root),
    }


def recovery_demo(acceptance: Path, wav: Path) -> dict[str, Any]:
    store = acceptance / "recovery_store"
    if store.exists():
        shutil.rmtree(store)
    command = [
        sys.executable,
        str(ROOT / "run.py"),
        "transcode",
        "--wav",
        str(wav),
        "--store",
        str(store),
        "--tier",
        "killrecovery",
        "--rate",
        "44100",
        "--filter",
        "128",
        "--quant",
        "24",
        "--slice-samples",
        "512",
    ]
    process = subprocess.Popen(
        command,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        cwd=str(ROOT),
    )
    trace_path = store / "trace.jsonl"
    killed_after = 0
    deadline = time.time() + 60
    while time.time() < deadline and process.poll() is None:
        if trace_path.exists():
            records = trace_records(trace_path)
            killed_after = sum(
                record.get("action") == "slice_recomputed" for record in records
            )
            if killed_after >= 6:
                os.kill(process.pid, signal.SIGKILL)
                break
        time.sleep(0.01)
    process.wait(timeout=30)
    if process.stdout is not None:
        process.stdout.close()
    if process.stderr is not None:
        process.stderr.close()
    run_dir = find_run_dir(store, "killrecovery")
    committed_before = TranscodeEngine(store).store.list_committed_indices(run_dir)
    inode_before = {
        index: (run_dir / "slices" / f"{index:06d}" / "audio.pcm").stat().st_ino
        for index in committed_before
    }

    resumed = subprocess.run(
        command,
        cwd=str(ROOT),
        capture_output=True,
        text=True,
        timeout=120,
    )
    if resumed.returncode != 0:
        raise RuntimeError(resumed.stderr)
    payload = json.loads(resumed.stdout)
    inode_after = {
        index: (run_dir / "slices" / f"{index:06d}" / "audio.pcm").stat().st_ino
        for index in committed_before
    }
    audited = subprocess.run(
        [
            sys.executable,
            str(ROOT / "run.py"),
            "audit",
            "--wav",
            str(wav),
            "--store",
            str(store),
            "--tier",
            "killrecovery",
        ],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
        timeout=120,
    )
    audit_payload = json.loads(audited.stdout) if audited.returncode == 0 else {}
    return {
        "store": rel(store),
        "trace": rel(trace_path),
        "sigkill_returncode": process.returncode,
        "slice_records_before_kill": killed_after,
        "committed_before_resume": committed_before,
        "resume_payload": payload,
        "committed_inodes_unchanged": inode_before == inode_after,
        "audit_ok": bool(audit_payload.get("report", {}).get("ok")),
    }


def determinism_demo(acceptance: Path, wav: Path) -> dict[str, Any]:
    roots = [acceptance / "determinism" / "run_a", acceptance / "determinism" / "run_b"]
    results: list[tuple[str, str]] = []
    for root in roots:
        if root.exists():
            shutil.rmtree(root)
        engine = TranscodeEngine(root / "store")
        first = engine.catalog.configure("det", 24000, 32, 16, 256)
        engine.generate_tier(str(wav), first)
        second = engine.catalog.configure("det", 24000, 48, 16, 256)
        engine.generate_tier(str(wav), second)
        import hashlib

        results.append(
            (
                hashlib.sha256((root / "store" / "trace.jsonl").read_bytes()).hexdigest(),
                hashlib.sha256((root / "store" / "published.json").read_bytes()).hexdigest(),
            )
        )
    return {
        "trace_sha256_run_a": results[0][0],
        "trace_sha256_run_b": results[1][0],
        "manifest_sha256_run_a": results[0][1],
        "manifest_sha256_run_b": results[1][1],
        "bytewise_equal": results[0] == results[1],
    }


def query_isolation_demo(
    engine: TranscodeEngine, wav: Path, acceptance: Path
) -> dict[str, Any]:
    tier = engine.catalog.configure("isolation", 22050, 32, 16, 256)
    engine.generate_tier(str(wav), tier)
    observations = 0
    errors: list[str] = []
    stop = threading.Event()

    def writer() -> None:
        try:
            for quant in (24, 16, 24):
                next_tier = engine.catalog.configure(
                    "isolation", 22050, 32, quant, 256
                )
                engine.generate_tier(str(wav), next_tier)
        except BaseException as exc:
            errors.append(f"{type(exc).__name__}: {exc}")
        finally:
            stop.set()

    def reader() -> None:
        nonlocal observations
        try:
            while not stop.is_set():
                published = engine.store.query_tier("isolation")
                for item, pcm in zip(
                    published.sidecar["slices"], published.audio_slices
                ):
                    import hashlib

                    if item["content_hash"] != hashlib.sha256(pcm).hexdigest():
                        errors.append(f"audio/sidecar mismatch at {item['index']}")
                        return
                if published.param_version != published.sidecar["param_version"]:
                    errors.append("param version mismatch")
                    return
                observations += 1
        except BaseException as exc:
            errors.append(f"{type(exc).__name__}: {exc}")

    writer_thread = threading.Thread(target=writer)
    reader_thread = threading.Thread(target=reader)
    writer_thread.start()
    reader_thread.start()
    writer_thread.join(120)
    reader_thread.join(120)
    return {
        "observations": observations,
        "errors": len(errors),
        "error_samples": errors[:5],
        "store": rel(acceptance / "store"),
    }


def render_markdown(report: dict[str, Any]) -> str:
    tiers = report["tiers"]
    lines = [
        "# 实际验收结果",
        "",
        f"- Python: {report['environment']['python']}",
        f"- NumPy: {report['environment']['numpy']}",
        f"- PySide6: {report['environment']['pyside6']}",
        f"- 固定种子: {report['seed']}",
        f"- 主轨迹: `{report['trace']}`",
        f"- 恢复轨迹: `{report['recovery']['trace']}`",
        "",
        "## 分档与侧信道",
        "",
        "| 档位 | 参数版本 | 目标 Hz | L | bit | 切片数 | 版本哈希 | 审计 |",
        "|---|---:|---:|---:|---:|---:|---|---|",
    ]
    for tier in tiers:
        lines.append(
            f"| {tier['tier_id']} | {tier['param_version']} | {tier['target_rate']} | "
            f"{tier['filter_length']} | {tier['quant_bits']} | {tier['total_slices']} | "
            f"`{tier['tier_version_hash'][:16]}…` | {'通过' if tier['audit_ok'] else '失败'} |"
        )
    lines.extend(
        [
            "",
            "## 相位传播",
            "",
            f"- 滤波器长度变更后相位余量改变切片数：{report['phase_propagation']['phase_changed_slices']}",
            f"- 强制传播首片：{report['phase_propagation']['first_slice']}",
            f"- 首片传播链：`{report['phase_propagation']['first_chain']}`",
            f"- 末片传播链长度：{report['phase_propagation']['last_chain_length']}",
            "",
            "## 漂移诊断",
            "",
            f"- 侧信道陈旧：first={report['drift']['sidecar_stale']['details']['first_drift_slice']}，"
            f"stale_side={report['drift']['sidecar_stale']['details']['stale_side']}",
            f"- 缓冲陈旧：first={report['drift']['buffer_stale']['details']['first_drift_slice']}，"
            f"stale_side={report['drift']['buffer_stale']['details']['stale_side']}，"
            f"偏差字节={report['drift']['buffer_stale']['details']['report']['drifting_slices'][0]['deviation_bytes']}",
            "",
            "## 强杀恢复",
            "",
            f"- SIGKILL 返回码：{report['recovery']['sigkill_returncode']}",
            f"- 强杀前已提交：{len(report['recovery']['committed_before_resume'])} 片",
            f"- 恢复起点：{report['recovery']['resume_payload']['resume_index']}",
            f"- 已提交切片 inode 不变：{report['recovery']['committed_inodes_unchanged']}",
            f"- 恢复后逐片审计：{'通过' if report['recovery']['audit_ok'] else '失败'}",
            "",
            "## 确定性与查询隔离",
            "",
            f"- 两次相同操作序列轨迹逐字节一致：{report['determinism']['bytewise_equal']}",
            f"- 轨迹 SHA-256：`{report['determinism']['trace_sha256_run_a']}`",
            f"- 并发替换期间查询读数：{report['query_isolation']['observations']} 次，错误 {report['query_isolation']['errors']} 次",
            f"- 过期清单哈希返回：{report['query_isolation']['stale_error_code']}",
            "",
            "## 测试与界面",
            "",
            f"- unittest：{report['unittest']['tests_run']} 项，"
            f"{'通过' if report['unittest']['ok'] else '失败'}；日志 `{report['unittest']['log']}`",
            f"- 离屏 GUI 生成、审计与截图：{'通过' if report['gui']['ok'] else '失败'}；"
            f"截图 `{report['gui']['screenshot']}`",
            f"- GUI 轨迹：`{report['gui']['trace']}`",
            "",
        ]
    )
    return "\n".join(lines)


def main() -> int:
    import numpy
    import PySide6

    acceptance = ROOT / "artifacts" / "acceptance"
    if acceptance.exists():
        shutil.rmtree(acceptance)
    acceptance.mkdir(parents=True)
    unittest_result = unittest_demo(acceptance)
    fixtures = generate_fixtures(acceptance / "samples", seed=19093, duration=2.4)

    surround = acceptance / "samples" / "surround51_24bit_seed19093.wav"
    mono = acceptance / "samples" / "mono_16bit_seed19093.wav"
    mono24 = acceptance / "samples" / "mono_24bit_seed19093.wav"
    engine = TranscodeEngine(acceptance / "store")

    tier_rows: list[dict[str, Any]] = []
    results: dict[str, Any] = {}
    for tier_id, rate, filt, quant, slice_size, wav in (
        ("hi44", 44100, 48, 16, 1024, surround),
        ("mid24", 24000, 32, 24, 512, mono24),
        ("mono32", 32000, 64, 24, 512, mono),
    ):
        tier = engine.catalog.configure(tier_id, rate, filt, quant, slice_size)
        result = engine.generate_tier(str(wav), tier)
        audit = engine.audit_published_tier(str(wav), tier_id)
        results[tier_id] = result
        tier_rows.append(
            {
                "tier_id": tier_id,
                "param_version": tier.param_version,
                "target_rate": tier.target_rate,
                "filter_length": tier.filter_length,
                "quant_bits": tier.quant_bits,
                "slice_samples": tier.slice_samples,
                "total_slices": result.sidecar["total_slices"],
                "tier_version_hash": result.tier_version_hash,
                "run_id": result.run_id,
                "audit_ok": audit["ok"],
            }
        )

    hi = results["hi44"]
    hi_changed = engine.generate_tier(
        str(surround),
        engine.catalog.configure("hi44", 44100, 64, 16, 1024),
    )
    hi_audit = engine.audit_published_tier(str(surround), "hi44")
    phase_changed = sum(
        a["phase_remainder_num"] != b["phase_remainder_num"]
        for a, b in zip(hi.sidecar["slices"], hi_changed.sidecar["slices"])
    )
    trace = trace_records(engine.trace.path)
    changed_records = [
        record
        for record in trace
        if record.get("action") == "slice_recomputed"
        and record.get("tier_id") == "hi44"
        and record.get("param_version") == 2
    ]

    mid = results["mid24"]
    mid_changed = engine.generate_tier(
        str(mono24),
        engine.catalog.configure("mid24", 24000, 48, 24, 512),
    )
    mid_audit = engine.audit_published_tier(str(mono24), "mid24")
    tier_rows[1].update(
        {
            "param_version": 2,
            "filter_length": 48,
            "total_slices": mid_changed.sidecar["total_slices"],
            "tier_version_hash": mid_changed.tier_version_hash,
            "run_id": mid_changed.run_id,
            "audit_ok": mid_audit["ok"],
            "reused_audio_slices": mid_changed.reused_audio_slices,
            "regenerated_audio_slices": mid_changed.regenerated_audio_slices,
            "sidecar_recomputed_slices": mid_changed.sidecar_recomputed_slices,
        }
    )

    stale_engine = TranscodeEngine(acceptance / "stale_store")
    stale_tier = stale_engine.catalog.configure("stale", 24000, 32, 16, 256)
    stale_engine.generate_tier(str(mono), stale_tier)
    first_generation_hash = stale_engine.store.query_tier(
        "stale"
    ).generation_hash
    stale_engine.generate_tier(
        str(mono),
        stale_engine.catalog.configure("stale", 24000, 32, 24, 256),
    )
    try:
        stale_engine.store.query_tier(
            "stale", expected_manifest_hash=first_generation_hash
        )
        stale_code = "E_EXPECTED_FAILURE_NOT_RAISED"
    except TaskError as exc:
        stale_code = exc.code

    drift = drift_cases(engine, mid, mid_changed, acceptance)
    invalid_result = wav_invalid_demo(
        mono, acceptance / "invalid_wav" / "mono_bad_mask.wav"
    )
    recovery = recovery_demo(acceptance, surround)
    determinism = determinism_demo(acceptance, mono)
    isolation = query_isolation_demo(engine, mono, acceptance)
    gui = gui_smoke_demo(acceptance, surround)

    shutil.copyfile(engine.trace.path, acceptance / "trace.jsonl")
    main_sidecar = results["hi44"].sidecar
    contract = main_sidecar["estimation"]
    report: dict[str, Any] = {
        "ok": all(row["audit_ok"] for row in tier_rows)
        and hi_audit["ok"]
        and mid_audit["ok"]
        and recovery["audit_ok"]
        and determinism["bytewise_equal"]
        and isolation["errors"] == 0
        and drift["sidecar_stale"].get("code") == "E_SIDECAR_DRIFT"
        and drift["buffer_stale"].get("code") == "E_SIDECAR_DRIFT"
        and invalid_result.get("code") == "E_WAV_INVALID"
        and stale_code == "E_VERSION_STALE"
        and unittest_result["ok"]
        and gui["ok"],
        "seed": 19093,
        "environment": {
            "python": sys.version.split()[0],
            "numpy": numpy.__version__,
            "pyside6": PySide6.__version__,
        },
        "fixtures": [
            {**fixture, "path": rel(Path(fixture["path"]))}
            for fixture in fixtures
        ],
        "trace": rel(acceptance / "trace.jsonl"),
        "tiers": tier_rows,
        "phase_propagation": {
            "phase_changed_slices": phase_changed,
            "first_slice": changed_records[0]["slice_index"],
            "first_reason": changed_records[0]["reason"],
            "first_chain": changed_records[0]["propagation_chain"],
            "last_slice": changed_records[-1]["slice_index"],
            "last_chain_length": len(changed_records[-1]["propagation_chain"]),
            "reused_slices": hi_changed.reused_slices,
            "recomputed_slices": hi_changed.recomputed_slices,
            "audit_ok": hi_audit["ok"],
        },
        "audio_reuse_after_filter_change": {
            "reused_audio_slices": mid_changed.reused_audio_slices,
            "regenerated_audio_slices": mid_changed.regenerated_audio_slices,
            "sidecar_recomputed_slices": mid_changed.sidecar_recomputed_slices,
        },
        "drift": drift,
        "invalid_wav": invalid_result,
        "recovery": recovery,
        "determinism": determinism,
        "query_isolation": {
            **isolation,
            "stale_error_code": stale_code,
            "stale_store": rel(acceptance / "stale_store"),
        },
        "unittest": unittest_result,
        "gui": gui,
        "estimation_contract": {
            "observation_window": contract["observation_window"],
            "sync_markers": contract["sync_markers"],
            "truth_source": contract["truth_source"],
        },
    }
    write_json(acceptance / "report.json", report)
    (acceptance / "report.md").write_text(render_markdown(report), encoding="utf-8")
    rendered = json.dumps(report, ensure_ascii=False, sort_keys=True, indent=2) + "\n"
    (acceptance / "acceptance.stdout.json").write_text(rendered, encoding="utf-8")
    print(rendered, end="")
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

