from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from .constraints import load_constraints
from .errors import (
    E_BUSY,
    E_COMMIT_FAILED,
    E_RECOVERY_INVALID,
    WorkbenchError,
)
from .generator import SampleSpec, generate_sample, generate_suite
from .session import WorkbenchSession, resume_session


def _print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def _exit_code(error: WorkbenchError) -> int:
    return {
        E_BUSY: 2,
        E_RECOVERY_INVALID: 3,
        E_COMMIT_FAILED: 4,
    }.get(error.code, 1)


def _crash_hook(after_commits: int):
    def hook(manifest: dict[str, Any]) -> None:
        if len(manifest.get("commits", [])) >= after_commits:
            os._exit(137)

    return hook


def _add_common_solve_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--session", required=True, help="Session directory")
    parser.add_argument("--run-id", default="cli")
    parser.add_argument("--chunk-size", type=int, default=8)
    parser.add_argument(
        "--crash-after-commits",
        type=int,
        default=None,
        help="Verification-only hard crash after N committed intervals",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="motion-workbench",
        description="Deterministic BVH constraint solving workbench",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    solve = sub.add_parser("solve", help="Solve a BVH/constraint pair")
    solve.add_argument("--bvh", required=True)
    solve.add_argument("--constraints", required=True)
    _add_common_solve_args(solve)

    resume = sub.add_parser(
        "resume", help="Resume strictly from committed disk state"
    )
    _add_common_solve_args(resume)

    verify = sub.add_parser("verify", help="Verify the committed hash chain")
    verify.add_argument("--session", required=True)
    verify.add_argument("--expected-table-version", type=int)
    verify.add_argument("--expected-parameters-version")

    generate = sub.add_parser("generate", help="Generate deterministic samples")
    generate.add_argument("--out", default="samples")
    generate.add_argument("--frames", type=int, default=48)
    generate.add_argument("--joints", type=int, default=12)
    generate.add_argument(
        "--conflict",
        choices=["none", "ee_unreachable", "joint_limit"],
        default="none",
    )
    generate.add_argument("--seed", type=int, default=7)
    generate.add_argument("--suite", action="store_true")

    edit = sub.add_parser(
        "edit", help="Edit one constraint and incrementally re-solve"
    )
    edit.add_argument("--session", required=True)
    edit.add_argument("--constraint", required=True)
    edit.add_argument("--weight", type=float)
    edit.add_argument("--tolerance", type=float)
    edit.add_argument("--chunk-size", type=int, default=8)
    edit.add_argument("--run-id", default="cli-edit")

    sub.add_parser("gui", help="Launch the desktop workbench")
    return parser


def _solve_command(args: argparse.Namespace) -> int:
    hook = (
        _crash_hook(args.crash_after_commits)
        if args.crash_after_commits is not None
        else None
    )
    session = WorkbenchSession(
        args.bvh,
        args.constraints,
        args.session,
        post_commit_hook=hook,
    )
    report = session.solve(
        run_id=args.run_id, chunk_size=args.chunk_size
    )
    summary = report.to_dict()
    _print(summary)
    if report.status == "conflict":
        return 5
    if report.status == "cancelled":
        return 6
    return 0


def _resume_command(args: argparse.Namespace) -> int:
    hook = (
        _crash_hook(args.crash_after_commits)
        if args.crash_after_commits is not None
        else None
    )
    session = resume_session(args.session, post_commit_hook=hook)
    report = session.solve(
        run_id=args.run_id, chunk_size=args.chunk_size
    )
    _print(report.to_dict())
    if report.status == "conflict":
        return 5
    return 0


def _verify_command(args: argparse.Namespace) -> int:
    session = resume_session(args.session)
    state = session.store.load_committed(
        session.bvh,
        constraints=session.constraints,
        expected_table_version=args.expected_table_version,
        expected_parameters_version=args.expected_parameters_version,
    )
    _print(
        {
            "ok": True,
            "chain_head": state.chain_head,
            "table_version": state.table_version,
            "parameters_version": state.parameters_version,
            "committed_ranges": [
                list(pair) for pair in state.committed_ranges
            ],
            "missing_ranges": [list(pair) for pair in state.missing_ranges],
            "next_frame": state.next_frame,
            "stale_frames": list(state.stale_frames),
            "committed_frame_count": len(state.frame_map),
        }
    )
    return 0


def _edit_command(args: argparse.Namespace) -> int:
    session = resume_session(args.session)
    constraints = session.constraints
    target = constraints.by_id.get(args.constraint)
    if target is None:
        raise WorkbenchError(
            "E_CONSTRAINT_INVALID",
            "Constraint not found",
            {"constraint_id": args.constraint},
        )
    changes: dict[str, Any] = {}
    if args.weight is not None:
        changes["weight"] = args.weight
    if args.tolerance is not None:
        changes["tolerance"] = args.tolerance
    if not changes:
        raise WorkbenchError(
            "E_CONSTRAINT_INVALID",
            "No weight or tolerance change supplied",
            {"constraint_id": args.constraint},
        )
    updated = target.with_changes(**changes)
    session.set_constraints(constraints.with_constraint(updated))
    report = session.solve(
        run_id=args.run_id, chunk_size=args.chunk_size
    )
    _print(report.to_dict())
    return 0


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "solve":
            return _solve_command(args)
        if args.command == "resume":
            return _resume_command(args)
        if args.command == "verify":
            return _verify_command(args)
        if args.command == "edit":
            return _edit_command(args)
        if args.command == "generate":
            if args.suite:
                results = generate_suite(args.out, seed=args.seed)
            else:
                results = [
                    generate_sample(
                        args.out,
                        SampleSpec(
                            frames=args.frames,
                            joints=args.joints,
                            conflict=args.conflict,
                            seed=args.seed,
                        ),
                    )
                ]
            _print(
                [
                    {
                        "bvh": str(result["bvh_path"]),
                        "constraints": str(result["constraints_path"]),
                        "baseline": str(result["baseline_path"]),
                        "status": result["baseline"]["status"],
                        "residual_digest": result["baseline"]["residual_digest"],
                    }
                    for result in results
                ]
            )
            return 0
        if args.command == "gui":
            from .gui import main as gui_main

            return gui_main([])
        raise AssertionError(args.command)
    except WorkbenchError as exc:
        _print(exc.to_dict())
        return _exit_code(exc)


if __name__ == "__main__":
    raise SystemExit(main())
