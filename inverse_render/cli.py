"""Command-line entry points for local offline inverse rendering."""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import threading
from collections import OrderedDict
from pathlib import Path
from typing import Any, Sequence

import numpy as np
import torch

from .checkpoint import CheckpointManager
from .engine import IncrementalEngine
from .errors import ErrorCode, ToolError
from .fit import FitConfig, FitResult, run_fit
from .generate import PRESETS, generate_samples
from .graph import build_dependency_graph
from .hashing import atomic_write_bytes, atomic_write_json, canonical_json_bytes, file_sha256
from .params import ParameterStore, load_group_specs
from .rendering import full_render
from .scene import load_scene
from .serialization import gradients_to_ordered, render_to_ordered
from .snapshot import make_snapshot


EXIT_CODES = {
    ErrorCode.E_BUSY: 3,
    ErrorCode.E_SNAPSHOT_STALE: 4,
    ErrorCode.E_CKPT_INVALID: 5,
    ErrorCode.E_REUSE_MISMATCH: 6,
}


def _print_json(value: Any, *, pretty: bool = True) -> None:
    if pretty:
        text = json.dumps(value, ensure_ascii=False, indent=2)
    else:
        text = canonical_json_bytes(value).decode("utf-8")
    print(text)


def _load(args: argparse.Namespace):
    scene = load_scene(args.scene)
    groups = load_group_specs(scene, args.groups)
    return scene, groups


def _environment_snapshot() -> OrderedDict[str, Any]:
    import pytest

    tensor = torch.tensor([1.0, 2.0], dtype=torch.float64)
    return OrderedDict(
        (
            ("python", sys.version.split()[0]),
            ("numpy", np.__version__),
            ("torch", torch.__version__),
            ("pytest", pytest.__version__),
            ("torch_cpu_only", torch.version.cuda is None),
            ("threads", torch.get_num_threads()),
            ("writable_cwd", os.access(Path.cwd(), os.W_OK)),
            ("finite_check", bool(torch.isfinite(tensor).all())),
        )
    )


def _command_env_check(args: argparse.Namespace) -> OrderedDict[str, Any]:
    result = _environment_snapshot()
    _print_json(result)
    return result


def _command_generate(args: argparse.Namespace) -> OrderedDict[str, Any]:
    result = generate_samples(args.out, seed=args.seed)
    _print_json(result)
    return result


def _command_render(args: argparse.Namespace) -> OrderedDict[str, Any]:
    scene, groups = _load(args)
    store = ParameterStore(scene, groups)
    graph = build_dependency_graph(scene, groups)
    nodes, loss = full_render(store, scene)
    forward = render_to_ordered(
        scene_name=scene.name,
        resolution=scene.resolution,
        nodes=nodes,
        loss=loss,
    )
    result: OrderedDict[str, Any] = OrderedDict(
        (("schema_version", 1), ("forward", forward))
    )
    if not args.no_gradients:
        store.zero_grad()
        loss.backward()
        gradients = gradients_to_ordered(
            group_order=store.group_order,
            parameter_order=store.parameter_order,
            gradients=store.group_gradients(),
            loss=float(loss.detach().cpu().item()),
        )
        result["gradients"] = gradients
    if args.image_out:
        rgb = nodes["lighting"]["rgb"].detach().cpu().numpy()
        buffer = Path(args.image_out)
        tmp = buffer.with_name(buffer.name + ".tmp")
        with open(tmp, "wb") as handle:
            np.save(handle, np.ascontiguousarray(rgb, dtype=np.float64), allow_pickle=False)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, buffer)
    result["graph_version"] = graph.version
    if args.output:
        atomic_write_json(Path(args.output), result)
        print(json.dumps({"ok": True, "output": str(Path(args.output).resolve())}))
    else:
        _print_json(result)
    return result


def _command_graph(args: argparse.Namespace) -> OrderedDict[str, Any]:
    scene, groups = _load(args)
    graph = build_dependency_graph(scene, groups)
    result: OrderedDict[str, Any] = OrderedDict(
        (
            ("schema_version", 1),
            ("graph_version", graph.version),
            ("nodes", list(graph.nodes)),
            ("edges", [edge.to_ordered() for edge in graph.edges]),
        )
    )
    if args.previous_groups:
        previous_groups = load_group_specs(scene, args.previous_groups)
        previous = build_dependency_graph(scene, previous_groups)
        result["edge_changes"] = graph.diff(previous).to_ordered()
    _print_json(result)
    return result


def _command_fit(args: argparse.Namespace) -> FitResult:
    cancel_event = threading.Event()
    timer = None
    if args.cancel_after is not None:
        timer = threading.Timer(args.cancel_after, cancel_event.set)
        timer.daemon = True
        timer.start()
    config = FitConfig(
        steps=args.steps,
        learning_rate=args.lr,
        schedule=args.schedule,
        resume=not args.no_resume,
        fresh=args.fresh,
        cancel_event=cancel_event,
        crash_after=args.crash_after,
        allow_process_exit=args.crash_after is not None,
    )
    try:
        result = run_fit(args.scene, args.groups, args.state_dir, config)
    finally:
        if timer is not None:
            timer.cancel()
    _print_json(result.to_ordered())
    return result


def _command_query(args: argparse.Namespace) -> OrderedDict[str, Any]:
    scene, groups = _load(args)
    graph = build_dependency_graph(scene, groups)
    manager = CheckpointManager(
        args.state_dir,
        scene_hash=scene.scene_hash(),
        expected_graph_version=graph.version,
        expected_group_order=[group.id for group in groups],
        expected_group_parameters={
            group.id: group.parameters for group in groups
        },
    )
    resume = manager.load()
    if resume is None:
        raise ToolError(
            ErrorCode.E_CKPT_INVALID,
            "no checkpoint is available",
            step=-1,
        )
    try:
        ParameterStore.from_checkpoint(scene, groups, resume.groups)
    except ToolError as exc:
        if exc.code == ErrorCode.E_CKPT_INVALID:
            raise
        raise ToolError(
            ErrorCode.E_CKPT_INVALID,
            "checkpoint parameter values are invalid",
            details=exc.details,
            step=resume.step,
        ) from exc
    snapshot = make_snapshot(
        step=resume.step,
        groups=resume.groups,
        last_loss=resume.last_loss,
        cumulative_loss=resume.cumulative_loss,
        graph_version=resume.graph_version,
        cache_hashes=resume.cache_hashes,
        checkpoint_hash=resume.checkpoint_hash,
        published=True,
    )
    _print_json(snapshot.to_ordered())
    return snapshot.to_ordered()


def _parse_values(raw: str) -> dict[str, Any]:
    try:
        value = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ToolError(
            ErrorCode.E_CONFIG_INVALID,
            "--values must be valid JSON",
            details={"raw": raw},
        ) from exc
    if not isinstance(value, dict):
        raise ToolError(
            ErrorCode.E_CONFIG_INVALID,
            "--values JSON must be an object mapping parameter paths to values",
        )
    return value


def _command_compare(args: argparse.Namespace) -> OrderedDict[str, Any]:
    scene, groups = _load(args)
    store = ParameterStore(scene, groups)
    graph = build_dependency_graph(scene, groups)
    engine = IncrementalEngine(scene, store, graph)
    initial = engine.evaluate()
    engine.backward(initial)
    if args.group:
        if args.values is None:
            raise ToolError(
                ErrorCode.E_CONFIG_INVALID, "--group requires --values"
            )
        values = _parse_values(args.values)
        store.update_group(
            args.group,
            values,
            increment_version=not args.stale_injection,
        )
    evaluation = engine.evaluate()
    gradient_result = engine.backward(evaluation)
    comparison = engine.compare_with_full(gradient_result)
    result = OrderedDict(
        (
            ("schema_version", 1),
            ("modified_group", args.group),
            ("stale_injection", bool(args.stale_injection)),
            (
                "evaluation",
                OrderedDict(
                    (
                        ("changed_groups", list(evaluation.changed_groups)),
                        ("affected_nodes", list(evaluation.affected_nodes)),
                        ("reused_nodes", list(evaluation.reused_nodes)),
                        ("recomputed_nodes", list(evaluation.recomputed_nodes)),
                        ("degraded", evaluation.degraded),
                        ("degraded_reason", evaluation.degraded_reason),
                        ("stale_nodes", list(evaluation.stale_nodes)),
                        ("ui_badge", "FULL_RECOMPUTE" if evaluation.degraded else "OK"),
                        ("content_hash", evaluation.content_hash),
                    )
                ),
            ),
            ("comparison", comparison.to_ordered()),
        )
    )
    if args.output:
        atomic_write_json(Path(args.output), result)
    else:
        _print_json(result)
    return result


def _run_cli_subprocess(arguments: list[str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-m", "inverse_render.cli", *arguments],
        cwd=str(Path(__file__).resolve().parent.parent),
        text=True,
        capture_output=True,
        check=False,
    )


def _command_demo(args: argparse.Namespace) -> OrderedDict[str, Any]:
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    samples = out / "samples"
    generated = generate_samples(samples, seed=args.seed)
    medium = samples / "coupled_medium"
    scene = load_scene(medium / "scene.json")
    groups = load_group_specs(scene, medium / "groups.json")
    graph = build_dependency_graph(scene, groups)
    store = ParameterStore(scene, groups)
    engine = IncrementalEngine(scene, store, graph)
    initial = engine.evaluate()
    engine.backward(initial)
    store.update_group(
        "material.primary",
        {"material.primary.albedo": [0.45, 0.38, 0.22]},
    )
    incremental_eval = engine.evaluate()
    incremental_grad = engine.backward(incremental_eval)
    reuse_comparison = engine.compare_with_full(incremental_grad)

    with torch.no_grad():
        store.parameters["material.accent.albedo"].add_(0.125)
    stale_eval = engine.evaluate()
    stale_grad = engine.backward(stale_eval)
    stale_comparison = engine.compare_with_full(stale_grad)

    small = samples / "coupled_small"
    fresh_state = out / "fit_fresh"
    fresh_args = [
        "fit",
        "--scene",
        str(small / "scene.json"),
        "--groups",
        str(small / "groups.json"),
        "--state-dir",
        str(fresh_state),
        "--steps",
        str(args.steps),
        "--lr",
        "0.02",
        "--schedule",
        "rotate",
        "--fresh",
    ]
    fresh_process = _run_cli_subprocess(fresh_args)
    if fresh_process.returncode != 0:
        raise ToolError(
            ErrorCode.E_IO,
            "demo fresh fit failed",
            details={
                "returncode": fresh_process.returncode,
                "stdout": fresh_process.stdout,
                "stderr": fresh_process.stderr,
            },
        )

    crash_state = out / "fit_crash_resume"
    crash_args = [
        "fit",
        "--scene",
        str(small / "scene.json"),
        "--groups",
        str(small / "groups.json"),
        "--state-dir",
        str(crash_state),
        "--steps",
        str(args.steps),
        "--lr",
        "0.02",
        "--schedule",
        "rotate",
        "--fresh",
        "--crash-after",
        "2",
    ]
    crashed = _run_cli_subprocess(crash_args)
    resume_args = [
        "fit",
        "--scene",
        str(small / "scene.json"),
        "--groups",
        str(small / "groups.json"),
        "--state-dir",
        str(crash_state),
        "--steps",
        str(args.steps),
        "--lr",
        "0.02",
        "--schedule",
        "rotate",
    ]
    resumed = _run_cli_subprocess(resume_args)
    if resumed.returncode != 0:
        raise ToolError(
            ErrorCode.E_IO,
            "demo resume failed",
            details={
                "returncode": resumed.returncode,
                "stdout": resumed.stdout,
                "stderr": resumed.stderr,
            },
        )
    resume_payload = json.loads(resumed.stdout)
    checkpoint = json.loads((crash_state / "checkpoint.json").read_text())
    history_steps = [entry["step"] for entry in checkpoint["history"]]
    trace_steps = [
        json.loads(line)["step"]
        for line in (crash_state / "trace.jsonl").read_text().splitlines()
        if line
    ]
    no_duplicate_commits = history_steps == list(range(args.steps + 1))
    no_duplicate_trace_steps = trace_steps == list(range(args.steps + 1))

    deterministic_state = out / "determinism_check"
    deterministic_payloads: list[dict[str, str]] = []
    for run_index in range(2):
        run_state = deterministic_state / f"run_{run_index}"
        process = _run_cli_subprocess(
            [
                "fit",
                "--scene",
                str(small / "scene.json"),
                "--groups",
                str(small / "groups.json"),
                "--state-dir",
                str(run_state),
                "--steps",
                "3",
                "--lr",
                "0.02",
                "--schedule",
                "rotate",
                "--fresh",
            ]
        )
        if process.returncode != 0:
            raise ToolError(
                ErrorCode.E_IO,
                "demo determinism run failed",
                details={"stderr": process.stderr},
            )
        deterministic_payloads.append(
            {
                "checkpoint_sha256": file_sha256(run_state / "checkpoint.json"),
                "trace_sha256": file_sha256(run_state / "trace.jsonl"),
            }
        )

    report = OrderedDict(
        (
            ("schema_version", 1),
            ("seed", int(args.seed)),
            ("environment", _environment_snapshot()),
            ("generated", generated),
            (
                "incremental_reuse",
                OrderedDict(
                    (
                        ("graph_version", graph.version),
                        ("matched", reuse_comparison.matched),
                        ("loss_abs_error", reuse_comparison.loss_abs_error),
                        ("max_group_grad_error", reuse_comparison.max_group_grad_error),
                        ("affected_nodes", list(incremental_eval.affected_nodes)),
                        ("reused_nodes", list(incremental_eval.reused_nodes)),
                        ("recomputed_nodes", list(incremental_eval.recomputed_nodes)),
                    )
                ),
            ),
            (
                "stale_cache_rejection",
                OrderedDict(
                    (
                        ("degraded", stale_eval.degraded),
                        ("degraded_reason", stale_eval.degraded_reason),
                        ("stale_nodes", list(stale_eval.stale_nodes)),
                        ("ui_badge", "FULL_RECOMPUTE" if stale_eval.degraded else "OK"),
                        ("matched_after_fallback", stale_comparison.matched),
                    )
                ),
            ),
            (
                "fresh_fit",
                OrderedDict(
                    (
                        ("completed_step", json.loads(fresh_process.stdout)["completed_step"]),
                        ("checkpoint_sha256", file_sha256(fresh_state / "checkpoint.json")),
                        ("trace_sha256", file_sha256(fresh_state / "trace.jsonl")),
                    )
                ),
            ),
            (
                "forced_termination",
                OrderedDict(
                    (
                        ("crash_returncode", crashed.returncode),
                        ("resumed_from", resume_payload["resumed_from"]),
                        ("completed_step", resume_payload["completed_step"]),
                        ("steps_executed_after_resume", resume_payload["steps_executed"]),
                        ("history_steps", history_steps),
                        ("trace_steps", trace_steps),
                        ("no_duplicate_commits", no_duplicate_commits),
                        ("no_duplicate_trace_steps", no_duplicate_trace_steps),
                        (
                            "checkpoint_equals_uninterrupted",
                            file_sha256(crash_state / "checkpoint.json")
                            == file_sha256(fresh_state / "checkpoint.json"),
                        ),
                        (
                            "trace_equals_uninterrupted",
                            file_sha256(crash_state / "trace.jsonl")
                            == file_sha256(fresh_state / "trace.jsonl"),
                        ),
                    )
                ),
            ),
            (
                "byte_determinism",
                OrderedDict(
                    (
                        ("runs", deterministic_payloads),
                        (
                            "checkpoint_identical",
                            deterministic_payloads[0]["checkpoint_sha256"]
                            == deterministic_payloads[1]["checkpoint_sha256"],
                        ),
                        (
                            "trace_identical",
                            deterministic_payloads[0]["trace_sha256"]
                            == deterministic_payloads[1]["trace_sha256"],
                        ),
                    )
                ),
            ),
        )
    )
    atomic_write_json(out / "demo_report.json", report)
    _print_json(report)
    return report


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="inverse-render",
        description="Deterministic local differentiable inverse rendering",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    env_parser = subparsers.add_parser("env-check", help="print runtime versions")
    env_parser.set_defaults(handler=_command_env_check)

    generate_parser = subparsers.add_parser("generate", help="generate local samples")
    generate_parser.add_argument("--out", required=True)
    generate_parser.add_argument("--seed", type=int, default=20261121)
    generate_parser.set_defaults(handler=_command_generate)

    render_parser = subparsers.add_parser("render", help="render and serialize forward/gradients")
    render_parser.add_argument("--scene", required=True)
    render_parser.add_argument("--groups", default=None)
    render_parser.add_argument("--image-out", default=None)
    render_parser.add_argument("--output", default=None)
    render_parser.add_argument("--no-gradients", action="store_true")
    render_parser.set_defaults(handler=_command_render)

    graph_parser = subparsers.add_parser("graph", help="build graph and edge changes")
    graph_parser.add_argument("--scene", required=True)
    graph_parser.add_argument("--groups", default=None)
    graph_parser.add_argument("--previous-groups", default=None)
    graph_parser.set_defaults(handler=_command_graph)

    fit_parser = subparsers.add_parser("fit", help="run cancellable grouped fitting")
    fit_parser.add_argument("--scene", required=True)
    fit_parser.add_argument("--groups", default=None)
    fit_parser.add_argument("--state-dir", required=True)
    fit_parser.add_argument("--steps", type=int, required=True)
    fit_parser.add_argument("--lr", type=float, default=0.02)
    fit_parser.add_argument("--schedule", default="all")
    fit_parser.add_argument("--fresh", action="store_true")
    fit_parser.add_argument("--no-resume", action="store_true")
    fit_parser.add_argument("--cancel-after", type=float, default=None)
    fit_parser.add_argument("--crash-after", type=int, default=None)
    fit_parser.set_defaults(handler=_command_fit)

    query_parser = subparsers.add_parser("query", help="validate and print latest checkpoint snapshot")
    query_parser.add_argument("--scene", required=True)
    query_parser.add_argument("--groups", default=None)
    query_parser.add_argument("--state-dir", required=True)
    query_parser.set_defaults(handler=_command_query)

    compare_parser = subparsers.add_parser("compare", help="compare incremental reuse with full recompute")
    compare_parser.add_argument("--scene", required=True)
    compare_parser.add_argument("--groups", default=None)
    compare_parser.add_argument("--group", default=None)
    compare_parser.add_argument("--values", default=None)
    compare_parser.add_argument("--stale-injection", action="store_true")
    compare_parser.add_argument("--output", default=None)
    compare_parser.set_defaults(handler=_command_compare)

    demo_parser = subparsers.add_parser("demo", help="run the complete acceptance demonstration")
    demo_parser.add_argument("--out", required=True)
    demo_parser.add_argument("--seed", type=int, default=20261121)
    demo_parser.add_argument("--steps", type=int, default=4)
    demo_parser.set_defaults(handler=_command_demo)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        result = args.handler(args)
        if isinstance(result, FitResult):
            return 0
        return 0
    except ToolError as exc:
        sys.stderr.write(canonical_json_bytes(exc.to_dict()).decode("utf-8") + "\n")
        return EXIT_CODES.get(exc.code, 2)
    except Exception as exc:
        error = OrderedDict(
            (
                ("ok", False),
                ("code", "E_INTERNAL"),
                ("message", str(exc)),
                ("type", type(exc).__name__),
            )
        )
        sys.stderr.write(canonical_json_bytes(error).decode("utf-8") + "\n")
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

