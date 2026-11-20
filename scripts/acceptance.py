"""Reproducible end-to-end acceptance run.

Usage:
    python scripts/acceptance.py --out artifacts/acceptance
    python scripts/acceptance.py --out artifacts/acceptance_full --full
"""

from __future__ import annotations

import argparse
import json
import multiprocessing
import platform
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

import numpy
import scipy
from PyQt5.QtCore import PYQT_VERSION_STR

from game_audio_restorer.recipes import load_recipe
from game_audio_restorer.restore import RestorationEngine
from game_audio_restorer.samples import generate_dataset
from game_audio_restorer.snapshot import load_and_validate
from game_audio_restorer.utils import sha256_json
from game_audio_restorer.wav_io import atomic_write_bytes, read_wav


def _checkpoint_then_pause(
    source: str, recipe_path: str, work: str, marker: str
) -> None:
    marker_path = Path(marker)

    def after_commit(_entry: dict[str, Any]) -> None:
        marker_path.write_text("committed\n", encoding="utf-8")
        time.sleep(60)

    engine = RestorationEngine(source, load_recipe(recipe_path), work)
    engine.run(on_segment=after_commit)


def _force_kill_recovery(out: Path) -> dict[str, Any]:
    source = out / "samples" / "sr48000_ch2.wav"
    if not source.exists():
        source = min((out / "samples").glob("*.wav"))
    recipe_path = out / "recipes" / "v2.json"
    recipe = load_recipe(recipe_path)
    work = out / "force-kill-recovery"
    marker = out / "force-kill.marker"
    context = multiprocessing.get_context("spawn")
    process = context.Process(
        target=_checkpoint_then_pause,
        args=(str(source), str(recipe_path), str(work), str(marker)),
    )
    process.start()
    deadline = time.monotonic() + 30
    while not marker.exists() and process.is_alive() and time.monotonic() < deadline:
        time.sleep(0.02)
    if not marker.exists():
        process.terminate()
        process.join(10)
        raise RuntimeError("force-kill worker did not reach the checkpoint")
    process.terminate()
    process.join(10)
    if process.is_alive():
        process.kill()
        process.join(10)
    partial = load_and_validate(
        work,
        expected_source_sha256=read_wav(source).sha256,
        expected_recipe_hash=recipe.hash(),
    )
    pre_report = (work / "report.json").exists()
    pre_segments = len(partial["segments"])
    committed_path = work / partial["segments"][0]["path"]
    committed_before = committed_path.read_bytes()
    resumed = RestorationEngine(source, recipe, work).run()
    committed_after = committed_path.read_bytes()
    report = json.loads(Path(resumed.report_path).read_text("utf-8"))
    return {
        "source": source.relative_to(out).as_posix(),
        "recipe": "v2",
        "exit_code": process.exitcode,
        "report_before_resume": pre_report,
        "committed_before_resume": pre_segments,
        "committed_after_resume": resumed.committed_segments,
        "first_segment_unchanged": committed_before == committed_after,
        "final_segment_count": len(report["segments"]),
        "final_report": "force-kill-recovery/report.json",
        "final_hash_chain_head": resumed.hash_chain_head,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="artifacts/acceptance")
    parser.add_argument("--seed", type=int, default=190_093)
    parser.add_argument("--full", action="store_true")
    args = parser.parse_args()
    out = Path(args.out)
    rates = (8000, 16000, 44100, 48000) if args.full else (8000, 48000)
    channels = (1, 2)
    dataset = generate_dataset(
        out,
        seed=args.seed,
        sample_rates=rates,
        channel_layouts=channels,
        duration_s=1.8,
        segment_seconds=1.0,
    )
    forced = _force_kill_recovery(out)
    pytest_run = subprocess.run(
        [sys.executable, "-m", "pytest", "-q"],
        cwd=str(ROOT),
        capture_output=True,
        text=True,
        timeout=180,
        check=False,
    )
    atomic_write_bytes(
        out / "pytest.log",
        (pytest_run.stdout + pytest_run.stderr).encode("utf-8"),
    )
    verification: dict[str, Any] = {
        "verification_version": 1,
        "platform": platform.platform(),
        "python": platform.python_version(),
        "dependencies": {
            "numpy": numpy.__version__,
            "scipy": scipy.__version__,
            "PyQt5": PYQT_VERSION_STR,
        },
        "dataset": dataset,
        "force_kill_recovery": forced,
        "test_command": f"{Path(sys.executable).name} -m pytest -q",
        "test_returncode": pytest_run.returncode,
        "test_summary": pytest_run.stdout.strip().splitlines()[-1]
        if pytest_run.stdout.strip()
        else "",
        "checks": {
            "three_versions_generated": dataset["run_count"] == dataset["sample_count"] * 3,
            "comparison_reports_exist": all(
                (out / "runs" / sample_id / "comparison.json").exists()
                for sample_id in json.loads((out / "baseline.json").read_text("utf-8"))[
                    "sources"
                ]
            ),
            "force_kill_kept_one_segment": forced["committed_before_resume"] == 1,
            "force_kill_produced_no_exit_report": forced["report_before_resume"] is False,
            "resume_completed": forced["committed_after_resume"]
            == forced["final_segment_count"],
            "committed_segment_not_rewritten": forced["first_segment_unchanged"],
            "tests_passed": pytest_run.returncode == 0,
        },
        "hash": "",
    }
    verification["hash"] = sha256_json(
        {key: value for key, value in verification.items() if key != "hash"}
    )
    atomic_write_bytes(
        out / "verification.json",
        (
            json.dumps(
                verification,
                ensure_ascii=False,
                sort_keys=True,
                indent=2,
            )
            + "\n"
        ).encode("utf-8"),
    )
    print(json.dumps(verification, ensure_ascii=False, sort_keys=True, indent=2))
    return pytest_run.returncode


if __name__ == "__main__":
    raise SystemExit(main())
