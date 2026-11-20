"""Generate and verify the complete local acceptance evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from mixrec.engine import MixEngine
from mixrec.errors import MixError
from mixrec.generator import baseline_from_index, generate_samples
from mixrec.index import atomic_write_json, load_index, validate_index
from mixrec.models import MixConfig


def sha256_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def copy_file(source: Path, target: Path) -> None:
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)


def _prepare_root(path: Path, force: bool) -> None:
    if path.exists() and any(path.iterdir()):
        if not force:
            raise SystemExit(f"output directory is not empty: {path} (use --force to replace it)")
        shutil.rmtree(path)
    path.mkdir(parents=True, exist_ok=True)


def _budget_config(source: Path, destination: Path) -> Path:
    value: dict[str, Any] = json.loads(source.read_text(encoding="utf-8"))
    # ``destination`` lives in <root>/runs/budget, and fixtures are in
    # <root>/jobs/<job>; relative paths keep the acceptance config portable.
    value["tracks"] = [
        dict(track, path=str(Path("../../jobs/short_timeline") / track["path"]))
        for track in value["tracks"]
    ]
    value["output_dir"] = "output"
    value["trace_path"] = "trace.jsonl"
    value["scheduler"]["per_slice_budget_bytes"] = 1000
    value["scheduler"]["memory_budget_bytes"] = 128 * 1024
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return destination


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default="artifacts/acceptance")
    parser.add_argument("--seed", type=int, default=20261121)
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args(argv)
    out_label = Path(args.out)
    out = out_label.resolve()
    _prepare_root(out, args.force)
    (out / "traces").mkdir(parents=True, exist_ok=True)
    (out / "indexes").mkdir(parents=True, exist_ok=True)
    (out / "baseline").mkdir(parents=True, exist_ok=True)
    (out / "runs").mkdir(parents=True, exist_ok=True)

    manifest = generate_samples(out, seed=args.seed, with_baseline=False)
    mono_config = out / "jobs/mixed_rates_mono/config.json"
    stereo_config = out / "jobs/mixed_rates_stereo/config.json"
    short_config = out / "jobs/short_timeline/config.json"

    initial_trace = out / "traces/initial_mixed_rates_mono.jsonl"
    initial_output = out / "runs/initial/output"
    initial_engine = MixEngine.from_file(
        mono_config,
        output_dir_override=initial_output,
        trace_override=initial_trace,
    )
    initial_result = initial_engine.run(resume=False)
    if not initial_result.ok:
        raise SystemExit("initial render did not complete")
    initial_index = load_index(initial_engine.index_path)
    validate_index(initial_index, output_dir=initial_output, verify_files=True)
    copy_file(initial_engine.index_path, out / "indexes/initial_mixed_rates_mono.json")
    initial_trace_hash = sha256_file(initial_trace)
    initial_file_hashes = [item["file_sha256"] for item in initial_index["slices"]]
    baseline_from_index(
        root=out,
        job_id="mixed_rates_mono",
        config_relative="jobs/mixed_rates_mono/config.json",
        index=initial_index,
        trace_path=initial_trace,
    )

    replay_trace = out / "runs/replay/trace.jsonl"
    replay_output = out / "runs/replay/output"
    replay_engine = MixEngine.from_file(
        mono_config,
        output_dir_override=replay_output,
        trace_override=replay_trace,
    )
    replay_result = replay_engine.run(resume=False)
    if not replay_result.ok:
        raise SystemExit("deterministic replay did not complete")
    replay_index = load_index(replay_engine.index_path)
    determinism = {
        "content_hashes_equal": [
            item["content_sha256"] for item in initial_index["slices"]
        ]
        == [item["content_sha256"] for item in replay_index["slices"]],
        "file_hashes_equal": initial_file_hashes
        == [item["file_sha256"] for item in replay_index["slices"]],
        "trace_sha256_equal": initial_trace_hash == sha256_file(replay_trace),
    }

    for config, job_id in ((stereo_config, "mixed_rates_stereo"), (short_config, "short_timeline")):
        job_dir = out / "jobs" / job_id
        trace = job_dir / "render_trace.jsonl"
        engine = MixEngine.from_file(
            config,
            output_dir_override=job_dir / "output",
            trace_override=trace,
        )
        result = engine.run(resume=False)
        if not result.ok:
            raise SystemExit(f"{job_id} did not complete")
        baseline_from_index(
            root=out,
            job_id=job_id,
            config_relative=f"jobs/{job_id}/config.json",
            index=load_index(engine.index_path),
            trace_path=trace,
        )

    incremental_trace = out / "traces/incremental_update.jsonl"
    copy_file(initial_trace, incremental_trace)
    incremental_engine = MixEngine.from_file(
        mono_config,
        output_dir_override=initial_output,
        trace_override=incremental_trace,
    )
    incremental_index = incremental_engine.update_track(
        "ambience",
        mute_ranges=[[0.4, 0.44]],
    )
    validate_index(incremental_index, output_dir=initial_output, verify_files=True)
    copy_file(initial_engine.index_path, out / "indexes/incremental_mixed_rates_mono.json")
    changed = [
        index
        for index, (before, after) in enumerate(
            zip(initial_index["slices"], incremental_index["slices"])
        )
        if before["content_sha256"] != after["content_sha256"]
    ]
    incremental_summary = {
        "affected_content_hashes_changed": bool(changed),
        "affected_slices": changed,
        "index_version": int(incremental_index["index_version"]),
        "ok": changed == [4, 5, 6],
        "reused_slices": int(incremental_index["total_slices"]) - len(changed),
        "unaffected_content_hashes_reused": all(
            initial_index["slices"][i]["content_sha256"]
            == incremental_index["slices"][i]["content_sha256"]
            for i in range(len(initial_index["slices"]))
            if i not in changed
        ),
    }

    queue_trace = out / "traces/scheduling_wait_queue.jsonl"
    queue_output = out / "runs/queue/output"
    queue_config = MixConfig.from_file(
        mono_config,
        output_dir_override=queue_output,
    )
    queue_config.per_slice_budget_bytes = 100000
    queue_config.memory_budget_bytes = 60000
    queue_engine = MixEngine(queue_config)
    queue_result = queue_engine.run(
        resume=False,
        trace_path=queue_trace,
        append_trace=False,
    )
    queue_events = [json.loads(line) for line in queue_trace.read_text().splitlines()]
    scheduling_summary = {
        "ok": queue_result.ok,
        "budget_degraded_events": sum(
            1 for event in queue_events if event["event"] == "budget_degraded"
        ),
        "task_waiting_events": sum(1 for event in queue_events if event["event"] == "task_waiting"),
        "max_reserved_bytes": max(
            (int(event["bytes"]) for event in queue_events if event["event"] == "memory_reserved"),
            default=0,
        ),
        "committed_slices": int(queue_result.summary["committed_slices"]),
    }

    budget_dir = out / "runs/budget"
    budget_config = _budget_config(short_config, budget_dir / "config.json")
    budget_trace = out / "traces/budget_rejection.jsonl"
    budget_engine = MixEngine.from_file(
        budget_config,
        output_dir_override=budget_dir / "output",
        trace_override=budget_trace,
    )
    budget_error: MixError | None = None
    try:
        budget_engine.run(resume=False)
    except MixError as exc:
        budget_error = exc
    if budget_error is None or budget_error.code != "E_BUDGET_EXCEEDED":
        raise SystemExit("budget rejection was not reproduced")
    budget_summary = {
        "canonical_slice_directory_exists": (budget_dir / "output/slices").exists(),
        "error": budget_error.to_dict(),
        "failed_slice": budget_error.slice_index,
        "index_exists": (budget_dir / "output/index.json").exists(),
        "ok": not (budget_dir / "output/slices").exists()
        and not (budget_dir / "output/index.json").exists(),
    }

    recovery_trace = out / "traces/recovery_from_prefix.jsonl"
    recovery_output = out / "runs/recovery/output"
    prefix_engine = MixEngine.from_file(
        mono_config,
        output_dir_override=recovery_output,
        trace_override=recovery_trace,
    )
    prefix = prefix_engine.run(resume=False, max_slices=3)
    if not prefix.stopped_early or prefix.index["committed_slices"] != 3:
        raise SystemExit("recovery prefix was not produced")
    resumed_engine = MixEngine.from_file(
        mono_config,
        output_dir_override=recovery_output,
        trace_override=recovery_trace,
    )
    resumed = resumed_engine.run(resume=True)
    resumed_index = resumed.index or load_index(resumed_engine.index_path)
    validate_index(resumed_index, output_dir=recovery_output, verify_files=True)
    recovery_summary = {
        "complete": bool(resumed_index["complete"]),
        "content_hashes_match_initial": [
            item["content_sha256"] for item in resumed_index["slices"]
        ]
        == [item["content_sha256"] for item in initial_index["slices"]],
        "continuous": all(
            resumed_index["slices"][i]["end_sample"]
            == resumed_index["slices"][i + 1]["start_sample"]
            for i in range(len(resumed_index["slices"]) - 1)
        ),
        "index_version": int(resumed_index["index_version"]),
        "ok": resumed.ok
        and resumed_index["chain_head"] == initial_index["chain_head"],
        "resumed_from_slice": 3,
        "summary": resumed.summary,
    }

    corrupt_output = out / "runs/corrupt/output"
    shutil.copytree(recovery_output, corrupt_output)
    corrupt_index = load_index(corrupt_output / "index.json")
    victim = corrupt_output / corrupt_index["slices"][2]["file"]
    payload = bytearray(victim.read_bytes())
    payload[60] ^= 0x5A
    victim.write_bytes(payload)
    corrupt_index_bytes = (corrupt_output / "index.json").read_bytes()
    corrupt_engine = MixEngine.from_file(
        mono_config,
        output_dir_override=corrupt_output,
        trace_override=out / "runs/corrupt/trace.jsonl",
    )
    recovery_invalid = False
    try:
        corrupt_engine.run(resume=True)
    except MixError as exc:
        recovery_invalid = exc.code == "E_RECOVERY_INVALID"
    recovery_guard = {
        "ok": recovery_invalid
        and (corrupt_output / "index.json").read_bytes() == corrupt_index_bytes,
        "index_unchanged": (corrupt_output / "index.json").read_bytes() == corrupt_index_bytes,
        "error_code": "E_RECOVERY_INVALID" if recovery_invalid else None,
    }

    summary = {
        "schema_version": 1,
        "seed": args.seed,
        "generated_jobs": [item["job_id"] for item in manifest["jobs"]],
        "initial": {
            "job_id": "mixed_rates_mono",
            "summary": initial_result.summary,
            "trace_sha256": initial_trace_hash,
        },
        "determinism": determinism,
        "budget_rejection": budget_summary,
        "incremental_update": incremental_summary,
        "scheduling": scheduling_summary,
        "recovery": recovery_summary,
        "recovery_invalid_guard": recovery_guard,
        "trace_files": [
            "traces/initial_mixed_rates_mono.jsonl",
            "traces/budget_rejection.jsonl",
            "traces/incremental_update.jsonl",
            "traces/scheduling_wait_queue.jsonl",
            "traces/recovery_from_prefix.jsonl",
        ],
    }
    atomic_write_json(out / "summary.json", summary)
    readme = f"""# Acceptance evidence

Generated with:

```text
python scripts/acceptance.py --out {out_label.as_posix()} --seed {args.seed} --force
```

- `summary.json`: initial render, deterministic replay, budget rejection,
  incremental update, recovery and recovery-invalid guard results.
- `traces/`: deterministic JSONL decisions, memory waits and commit events.
- `indexes/`: complete initial and incrementally published hash chains.
- `baseline/`: content, WAV-file and trace hashes for all generated jobs.
- `jobs/`: generated local WAVs, config JSON and rendered outputs.

The traces contain no timestamps, thread identifiers or absolute paths.
"""
    (out / "README.md").write_text(readme, encoding="utf-8")
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
