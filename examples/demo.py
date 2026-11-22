"""End-to-end TickArena local demo.

Run from the repository root:

    python examples/demo.py
    python examples/demo.py --quick
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import sys
import time
from collections import Counter
from pathlib import Path
from typing import Any

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tickarena import (  # noqa: E402
    E_CANCELLED,
    E_OK,
    E_PAUSED,
    BoundedParallel,
    CheckpointStore,
    Engine,
    InMemoryEventSource,
    JsonlEventSource,
    make_initial_state,
    verify_replay,
)
from tickarena.generator import write_generated_match  # noqa: E402
from tickarena.replay import load_and_validate_replay  # noqa: E402
from tickarena.atomic import replace_with_retry  # noqa: E402


def _atomic_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(path.name + ".tmp")
    temp.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    replace_with_retry(temp, path)


def _prepare_runtime(base: Path) -> dict[str, Path]:
    paths = {
        "events": base / "events",
        "replays": base / "replays",
        "checkpoints": base / "checkpoints",
        "results": base / "results",
    }
    for path in paths.values():
        path.mkdir(parents=True, exist_ok=True)
    probe = paths["results"] / ".write_probe"
    probe.write_text("ok", encoding="utf-8")
    if probe.read_text(encoding="utf-8") != "ok":
        raise RuntimeError(f"persistent directory is not writable: {paths['results']}")
    probe.unlink()
    return paths


def _error_code_demo(paths: dict[str, Path]) -> dict[str, Any]:
    initial = make_initial_state(
        seed=4_242,
        players=2,
        tick_count=6,
        win_score=100,
    )
    initial["players"][1]["unit"]["resource"] = 0
    events = [
        {"tick": 0, "type": "skill", "actor": "u1", "skill": "bolt", "target": "u2"},
        {"tick": 0, "type": "skill", "actor": "u1", "skill": "bolt", "target": "u2"},
        {"tick": 0, "type": "skill", "actor": "u2", "skill": "bolt", "target": "u1"},
        {"tick": 0, "type": "skill", "actor": "ghost", "skill": "bolt", "target": "u1"},
        {"tick": 0, "type": "move", "actor": "u1", "dx": 1.0, "dy": 0},
        {"tick": 2, "type": "move", "actor": "u1", "dx": 1, "dy": 0},
        {"tick": 1, "type": "move", "actor": "u1", "dx": 1, "dy": 0},
    ]
    source = InMemoryEventSource(initial, events)
    replay = paths["replays"] / "error_codes.replay.jsonl"
    engine = Engine(initial, source, replay_path=replay, snapshot_interval=2)
    observed: Counter[str] = Counter()
    while not engine.done:
        step = engine.step()
        for summary in step.rejected:
            observed[summary["code"]] += int(summary.get("count", 1))
    engine.close()
    report = verify_replay(replay, source)
    return {
        "replay": str(replay),
        "final_tick": engine.state.tick,
        "observed_rejections": dict(sorted(observed.items())),
        "verification": report.code,
        "required_codes_present": all(
            code in observed for code in ("E_ORDER", "E_UNIT", "E_COOLDOWN", "E_RES")
        ),
    }


def _small_interactive_demo(
    paths: dict[str, Path],
    *,
    seed: int,
    ticks: int,
) -> dict[str, Any]:
    initial = make_initial_state(
        seed=seed,
        players=4,
        tick_count=ticks,
        win_score=1_000_000,
    )
    event_path = paths["events"] / "small.events.jsonl"
    write_generated_match(event_path, initial, event_density="1.25")
    replay = paths["replays"] / "small.replay.jsonl"
    store = CheckpointStore(paths["checkpoints"] / "small")
    source = JsonlEventSource(event_path)
    engine = Engine(
        initial,
        source,
        replay_path=replay,
        snapshot_interval=100,
        checkpoint_store=store,
        checkpoint_every=256,
    )
    started = time.perf_counter()
    rejection_counts: Counter[str] = Counter()
    while not engine.done:
        step = engine.step()
        if step.code != E_OK:
            raise RuntimeError(f"interactive step failed: {step}")
        for summary in step.rejected:
            rejection_counts[summary["code"]] += int(summary.get("count", 1))
    elapsed = time.perf_counter() - started
    final_tick = engine.state.tick
    final_result = engine.state.result
    engine.close()
    report = verify_replay(replay, event_path)
    meta = load_and_validate_replay(replay)
    return {
        "mode": "interactive_tick",
        "ticks": final_tick + 1,
        "elapsed_s": round(elapsed, 4),
        "event_fingerprint": meta.header["event_fingerprint"],
        "replay": str(replay),
        "result": final_result,
        "rejections": dict(sorted(rejection_counts.items())),
        "verification": report.code,
    }


def _run_large_with_pause(
    paths: dict[str, Path],
    *,
    seed: int,
    ticks: int,
) -> dict[str, Any]:
    initial = make_initial_state(
        seed=seed + 1,
        players=4,
        tick_count=ticks,
        win_score=1_000_000,
    )
    event_path = paths["events"] / "large.events.jsonl"
    write_generated_match(event_path, initial, event_density="0.8")
    replay = paths["replays"] / "large.replay.jsonl"
    store = CheckpointStore(paths["checkpoints"] / "large")
    source = JsonlEventSource(event_path)
    engine = Engine(
        initial,
        source,
        replay_path=replay,
        snapshot_interval=1_000,
        checkpoint_store=store,
        checkpoint_every=2_048,
    )
    started = time.perf_counter()
    rejection_counts: Counter[str] = Counter()
    batch_count = 0
    paused_tick: int | None = None
    resume_info: dict[str, Any] | None = None
    while not engine.done:
        result = engine.run_batch(1_024)
        batch_count += 1
        for code, count in result.rejection_counts.items():
            rejection_counts[code] += count
        if paused_tick is None and not engine.done and result.tick >= 8_192:
            engine.request_pause()
            paused = engine.run_batch(1_024)
            if paused.code != E_PAUSED:
                raise RuntimeError(f"expected E_PAUSED, got {paused}")
            paused_tick = paused.tick
            engine.close()
            resumed_source = JsonlEventSource(event_path)
            engine, info = Engine.resume(
                initial,
                resumed_source,
                checkpoint_store=store,
                replay_path=replay,
                snapshot_interval=1_000,
                checkpoint_every=2_048,
            )
            resume_info = {
                "resumed": info.resumed,
                "restarted": info.restarted,
                "code": info.code,
                "reason": info.reason,
                "tick": engine.state.tick,
            }
        if result.code not in {E_OK, "E_MORE"}:
            raise RuntimeError(f"large batch failed: {result}")
    elapsed = time.perf_counter() - started
    final_tick = engine.state.tick
    final_result = engine.state.result
    stats = engine.resource_stats()
    engine.close()
    report = verify_replay(replay, event_path)
    meta = load_and_validate_replay(replay)
    return {
        "mode": "batch",
        "ticks": final_tick + 1,
        "batches": batch_count,
        "elapsed_s": round(elapsed, 4),
        "event_fingerprint": meta.header["event_fingerprint"],
        "replay": str(replay),
        "event_path": str(event_path),
        "result": final_result,
        "rejections": dict(sorted(rejection_counts.items())),
        "pause_resume": {
            "paused_tick": paused_tick,
            "info": resume_info,
        },
        "resource_stats": stats,
        "verification": report.code,
    }


def _cancel_demo(paths: dict[str, Path]) -> dict[str, Any]:
    initial = make_initial_state(
        seed=9_001,
        players=2,
        tick_count=200,
        win_score=1_000_000,
    )
    event_path = paths["events"] / "cancel.events.jsonl"
    write_generated_match(event_path, initial, event_density="1.0")
    store = CheckpointStore(paths["checkpoints"] / "cancel")
    source = JsonlEventSource(event_path)
    engine = Engine(
        initial,
        source,
        replay_path=paths["replays"] / "cancel.replay.jsonl",
        checkpoint_store=store,
        checkpoint_every=32,
    )
    engine.run_batch(40)
    engine.request_cancel()
    result = engine.run_batch(64)
    checkpoint_tick = store.last_tick
    engine.close()
    return {
        "code": result.code,
        "expected": E_CANCELLED,
        "checkpoint_tick": checkpoint_tick,
        "tick": result.tick,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workdir", default=str(ROOT / "runtime"))
    parser.add_argument("--seed", type=int, default=2_026_1123)
    parser.add_argument("--small-ticks", type=int, default=5_000)
    parser.add_argument("--large-ticks", type=int, default=100_000)
    parser.add_argument("--quick", action="store_true")
    args = parser.parse_args(argv)
    if args.quick:
        args.small_ticks = 500
        args.large_ticks = 5_000

    paths = _prepare_runtime(Path(args.workdir))
    print("== TickArena local demo ==")
    print(
        json.dumps(
            {
                "python": platform.python_version(),
                "numpy": np.__version__,
                "workdir": str(Path(args.workdir).resolve()),
                "small_ticks": args.small_ticks,
                "large_ticks": args.large_ticks,
            },
            ensure_ascii=False,
        )
    )

    errors = _error_code_demo(paths)
    print("error-codes:", json.dumps(errors, ensure_ascii=False))

    small = _small_interactive_demo(
        paths,
        seed=args.seed,
        ticks=args.small_ticks,
    )
    print("small-interactive:", json.dumps(small, ensure_ascii=False))

    def simulate_large(_: str) -> dict[str, Any]:
        return _run_large_with_pause(
            paths,
            seed=args.seed,
            ticks=args.large_ticks,
        )

    def verify_small(_: str) -> str:
        return verify_replay(
            paths["replays"] / "small.replay.jsonl",
            paths["events"] / "small.events.jsonl",
        ).code

    results = BoundedParallel(max_workers=2, queue_limit=2).map(
        lambda job: job(),
        [lambda: simulate_large("large"), lambda: verify_small("small")],
    )
    large = results[0]
    small_parallel_verification = results[1]
    large_verification = verify_replay(
        large["replay"],
        large["event_path"],
    )
    print(
        "large-batch:",
        json.dumps(
            {
                **{key: value for key, value in large.items() if key != "resource_stats"},
                "resource_stats": large["resource_stats"],
                "small_reverified_in_parallel": small_parallel_verification,
                "final_verification": large_verification.code,
            },
            ensure_ascii=False,
        )
    )

    cancelled = _cancel_demo(paths)
    print("cancel:", json.dumps(cancelled, ensure_ascii=False))

    summary = {
        "environment": {
            "python": platform.python_version(),
            "numpy": np.__version__,
            "platform": platform.platform(),
        },
        "errors": errors,
        "small": small,
        "large": large,
        "large_verification": large_verification.code,
        "small_parallel_verification": small_parallel_verification,
        "cancel": cancelled,
    }
    _atomic_json(paths["results"] / "demo_summary.json", summary)
    print("summary:", paths["results"] / "demo_summary.json")

    expected_rejections = {"E_ORDER", "E_UNIT", "E_COOLDOWN", "E_RES"}
    passed = (
        errors["verification"] == E_OK
        and expected_rejections <= set(errors["observed_rejections"])
        and small["verification"] == E_OK
        and large_verification.code == E_OK
        and small_parallel_verification == E_OK
        and cancelled["code"] == E_CANCELLED
    )
    print("RESULT:", "PASS" if passed else "FAIL")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())

