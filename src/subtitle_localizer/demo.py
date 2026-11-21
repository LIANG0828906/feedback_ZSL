from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any

from .errors import BusinessError
from .generator import generate_corpus
from .hashing import pretty_json_bytes, write_bytes_atomic
from .layout import compare_layouts, compute_layout
from .models import DescriptionInput, LayoutParams, SentenceInput, SubtitleAsset
from .persistence import load_state
from .session import LayoutSession
from .timebase import read_frame_table, read_wav_info, validate_media_and_asset


def _project_src() -> Path:
    return Path(__file__).resolve().parents[1]


def _refresh_positions(duration_samples: int) -> list[int]:
    return [min(duration_samples - 1, index * 240_000) for index in range(0, 4)]


def _run_normal_case(case_dir: Path, trace_path: Path, first_trace: bool) -> dict[str, Any]:
    with LayoutSession.open(case_dir, trace_path=trace_path, truncate_trace=first_trace) as session:
        baseline = session.commit()
        _write_json(case_dir / "baseline.json", baseline)
        report = baseline
        refresh_count = 0
        for position in _refresh_positions(session._wav.frame_count):
            session.refresh(position)
            refresh_count += 1

        # Revision 2: tighten the global inter-sentence gap.
        session.stage_params({"min_gap_ms": 320})
        report = session.commit()
        for position in _refresh_positions(session._wav.frame_count)[1:3]:
            session.refresh(position)
            refresh_count += 1

        # Revision 3: move all speaker-specific picks for 5.1 material; for
        # mono material this is a no-op and a default-CPS edit is used instead.
        if session._wav.channels == 6:
            session.stage_params(
                {
                    "channel_map": {
                        "SPEAKER_A": 1,
                        "SPEAKER_B": 3,
                        "SPEAKER_C": 5,
                    }
                }
            )
        else:
            session.stage_params({"default_cps": _bumped_cps(session.params.default_cps)})
        report = session.commit()
        session.refresh(240_000)
        refresh_count += 1

        # Revision 4: change a single sentence's reading speed.  This is the
        # smallest useful incremental update and exercises hash reuse.
        target = next((item for item in session.asset.sentences if item.cps is not None), session.asset.sentences[-1])
        session.stage_sentence(target.id, {"cps": _bumped_cps(target.cps or session.params.default_cps)})
        report = session.commit()
        for position in _refresh_positions(session._wav.frame_count)[2:]:
            session.refresh(position)
            refresh_count += 1
        offline = session.compare_with_offline()
        _write_json(case_dir / "final_layout.json", report)
        return {
            "asset_id": report["asset_id"],
            "commit_hash": report["commit_hash"],
            "directory": case_dir.name,
            "offline_equal": True,
            "offline_commit_hash": offline["commit_hash"],
            "refresh_count": refresh_count,
            "revision": report["revision"],
            "reused_count": report["counts"]["reused"],
            "sentence_count": report["counts"]["sentences"],
        }


def _bumped_cps(value: str) -> str:
    try:
        number = int(value)
    except ValueError:
        number = 10
    return str(max(8, min(20, number + 1)))


def _run_recovery_case(case_dir: Path, trace_path: Path) -> dict[str, Any]:
    with LayoutSession.open(case_dir, trace_path=trace_path) as session:
        session.commit()
        session.stage_params({"min_gap_ms": 300})
        before = session.commit()
        before_hash = before["commit_hash"]
        before_revision = before["revision"]
        session.refresh(240_000)
    with LayoutSession.open(case_dir, trace_path=trace_path) as session:
        session.stage_params({"default_cps": _bumped_cps(session.params.default_cps)})
        session.stage_descriptions(
            [
                *[item.to_dict() for item in session.asset.descriptions],
                {
                    "after_sentence_id": session.asset.sentences[2].id,
                    "id": "D9001",
                    "max_cps": "9",
                    "min_duration_ms": 400,
                    "priority": 70,
                    "speaker": "NARRATOR",
                    "text": "恢复检查点后的描述轨必须只重算未完成句区间",
                },
            ]
        )
    env = dict(os.environ)
    source_path = str(_project_src())
    env["PYTHONPATH"] = source_path + (os.pathsep + env["PYTHONPATH"] if env.get("PYTHONPATH") else "")
    command = [
        sys.executable,
        "-m",
        "subtitle_localizer.cli",
        "_crash-worker",
        "--case",
        str(case_dir),
        "--trace",
        str(trace_path),
        "--after-checkpoints",
        "2",
    ]
    completed_process = subprocess.run(command, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if completed_process.returncode != 70:
        raise RuntimeError(
            "recovery worker did not terminate at the requested checkpoint: "
            f"{completed_process.returncode} {completed_process.stderr.decode('utf-8', 'replace')}"
        )
    with LayoutSession.open(case_dir, trace_path=trace_path) as session:
        info = session.pending_info()
        after = session.commit()
        offline = session.compare_with_offline()
        _write_json(case_dir / "final_layout.json", after)
        return {
            "after_commit_hash": after["commit_hash"],
            "after_revision": after["revision"],
            "asset_id": after["asset_id"],
            "before_commit_hash": before_hash,
            "before_revision": before_revision,
            "directory": case_dir.name,
            "exit_code": completed_process.returncode,
            "exit_report_generated": False,
            "offline_equal": True,
            "pending_unfinished_sentence_ids": info["unfinished_sentence_ids"],
            "resumed_affected_ids": after["recomputed_sentence_ids"],
            "resumed_reused_ids": after["reused_sentence_ids"],
        }


def _write_json(path: Path, value: Any) -> None:
    write_bytes_atomic(path, pretty_json_bytes(value))


def _error_code(callable_: Any) -> str:
    try:
        callable_()
    except BusinessError as exc:
        return exc.code
    return "missing"


def _run_error_checks(output_dir: Path, manifest: dict[str, Any]) -> dict[str, str]:
    case_dir = output_dir / manifest["cases"][0]["directory"]
    asset = SubtitleAsset.load(case_dir / "subtitles.json")
    wav = read_wav_info(case_dir / "program.wav")
    frames = read_frame_table(case_dir / "frames.ftbl")
    busy = {"code": None}
    with LayoutSession.open(case_dir, trace_path=output_dir / "trace.jsonl") as session:
        busy["code"] = _error_code(lambda: LayoutSession.open(case_dir, trace_path=output_dir / "trace.jsonl"))
        # Keep the session open until the nested-open check completes.
        del session

    bad_sentence = SentenceInput(
        id=asset.sentences[0].id,
        text=asset.sentences[0].text,
        start=asset.sentences[0].start,
        end="99:00:00.000000",
        speaker=asset.sentences[0].speaker,
    )
    bad_asset = asset.replace(sentences=(bad_sentence, *asset.sentences[1:]))
    timebase_code = _error_code(lambda: validate_media_and_asset(bad_asset, wav, frames))

    conflict_asset = SubtitleAsset(
        asset_id="conflict-check",
        sentences=(
            SentenceInput("C1", "第一句", "00:00:01.000000", "00:00:01.400000", "A"),
            SentenceInput("C2", "第二句", "00:00:02.300000", "00:00:02.600000", "B"),
        ),
        descriptions=(
            DescriptionInput(
                id="CD1",
                after_sentence_id="C1",
                text="描述" * 12,
                speaker="NARRATOR",
                max_cps="8",
                min_duration_ms=400,
                priority=50,
            ),
        ),
        parameters=LayoutParams(default_cps="10", min_gap_ms=200, sentence_priority=50),
    )
    conflict_code = _error_code(lambda: compute_layout(conflict_asset, wav, frames))

    offline = compute_layout(asset, wav, frames, revision=1, parent_hash="0" * 64)
    tampered = json.loads(json.dumps(offline))
    tampered["sentences"][0]["start_sample"] = int(tampered["sentences"][0]["start_sample"]) + 1
    mismatch_code = _error_code(lambda: compare_layouts(tampered, offline))

    recovery_code = "missing"
    state = load_state(case_dir / "layout_state.json")
    if state is not None:
        corrupted = json.loads(json.dumps(state))
        corrupted["history"][0]["parent_hash"] = "f" * 64
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "bad_state.json"
            _write_json(path, corrupted)
            try:
                load_state(path)
            except BusinessError as exc:
                recovery_code = exc.code

    return {
        "E_BUSY": busy["code"],
        "E_DESC_CONFLICT": conflict_code,
        "E_LAYOUT_MISMATCH": mismatch_code,
        "E_RECOVERY_INVALID": recovery_code,
        "E_TIMEBASE_INVALID": timebase_code,
    }


def run_demo(output_dir: Path | str, *, seed: int = 7, clean: bool = True) -> dict[str, Any]:
    output_dir = Path(output_dir)
    if clean and output_dir.exists():
        # Only the generated demo tree is replaced; the project source and any
        # user data outside this directory are untouched.
        shutil.rmtree(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    trace_path = output_dir / "trace.jsonl"
    write_bytes_atomic(trace_path, b"")
    manifest = generate_corpus(output_dir, seed=seed)
    reports: list[dict[str, Any]] = []
    refresh_count = 0
    for index, case in enumerate(manifest["cases"]):
        if case["recovery"]:
            result = _run_recovery_case(output_dir / case["directory"], trace_path)
        else:
            result = _run_normal_case(
                output_dir / case["directory"],
                trace_path,
                first_trace=(index == 0),
            )
        refresh_count += result.get("refresh_count", 0)
        reports.append(result)
    error_checks = _run_error_checks(output_dir, manifest)
    manifest_by_dir = {item["directory"]: item for item in manifest["cases"]}
    for result in reports:
        target = manifest_by_dir[result["directory"]]
        target["final_commit_hash"] = result.get("commit_hash") or result["after_commit_hash"]
        target["final_revision"] = result.get("revision") or result["after_revision"]
        target["offline_equal"] = result["offline_equal"]
        target["reused_count"] = len(result.get("resumed_reused_ids", [])) if "resumed_reused_ids" in result else result.get("reused_count", 0)
    _write_json(output_dir / "manifest.json", manifest)
    trace_bytes = trace_path.read_bytes()
    trace_rows = sum(1 for line in trace_bytes.splitlines() if line)
    demo_report = {
        "cases": reports,
        "error_checks": error_checks,
        "final_hashes": {
            item["asset_id"]: item.get("commit_hash") or item["after_commit_hash"]
            for item in reports
        },
        "refresh_count": refresh_count,
        "schema": "subtitle-demo-report/v1",
        "seed": seed,
        "trace_rows": trace_rows,
        "trace_sha256": hashlib.sha256(trace_bytes).hexdigest(),
    }
    _write_json(output_dir / "demo_report.json", demo_report)
    return demo_report

