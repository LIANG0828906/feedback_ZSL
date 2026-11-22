from __future__ import annotations

import argparse
import json
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

from .budget import SimulationRequest
from .checkpoint import SnapshotContext
from .compare import compare_results
from .errors import BalanceError, ErrorCode
from .generator import apply_project_injection, generate_all
from .report import build_report
from .scheduler import run_validation
from .validation import load_candidate_set, load_project, parse_project


def _json_print(payload: Any) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True))


def _request_from_args(args: argparse.Namespace) -> SimulationRequest:
    return SimulationRequest(
        target_samples=args.target_samples,
        min_samples=args.min_samples,
        total_sample_budget=args.total_sample_budget,
        interrupt_after_commits=getattr(args, "interrupt_after_commits", None),
    )


def _write_report_bundle(report: Any, output: str | None, markdown: str | None) -> None:
    if output:
        path = Path(output)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            json.dumps(report.to_dict(), ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    if markdown:
        path = Path(markdown)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(report.to_markdown(), encoding="utf-8")


def cmd_generate(args: argparse.Namespace) -> int:
    manifest = generate_all(args.output)
    _json_print(
        {
            "ok": True,
            "output": str(Path(args.output)),
            "manifest_hash": manifest["manifest_hash"],
            "projects": [
                {
                    "slug": item["slug"],
                    "project_hash": item["project_hash"],
                    "candidate_set_hash": item["candidate_set_hash"],
                }
                for item in manifest["projects"]
            ],
        }
    )
    return 0


def cmd_validate(args: argparse.Namespace) -> int:
    project = load_project(args.project)
    payload: dict[str, Any] = {
        "ok": True,
        "project_id": project.project_id,
        "content_hash": project.content_hash(),
        "classes": len(project.classes),
        "skills": len(project.skills),
        "formulas": len(project.formulas),
        "loadouts": len(project.loadouts),
        "scenarios": len(project.scenarios),
        "formula_dependencies": sum(
            len(formula.formula_dependencies()) for formula in project.formulas.values()
        ),
    }
    if args.candidates:
        candidate_set = load_candidate_set(args.candidates, project)
        payload["candidate_set_hash"] = candidate_set.content_hash()
        payload["candidates"] = len(candidate_set.candidates)
    _json_print(payload)
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    project = load_project(args.project)
    candidate_set = load_candidate_set(args.candidates, project)
    request = _request_from_args(args)
    result = run_validation(
        project,
        candidate_set,
        request,
        checkpoint_path=args.checkpoint,
        mode=args.mode,
        parallel_slots_override=args.parallel_slots,
    )
    report = build_report(project, candidate_set, result)
    _write_report_bundle(report, args.output, args.markdown)
    _json_print(
        {
            "ok": True,
            "project_id": project.project_id,
            "mode": result.mode,
            "interrupted": result.interrupted,
            "candidates": len(result.candidates),
            "submitted_candidates": sum(
                1 for item in result.candidates if item.state == "SUBMITTED"
            ),
            "combos": len(result.combos),
            "schedule": result.schedule.to_dict(),
            "summary": report.payload["summary"],
            "output": args.output,
            "markdown": args.markdown,
        }
    )
    return 0


def cmd_three_path(args: argparse.Namespace) -> int:
    project = load_project(args.project)
    candidate_set = load_candidate_set(args.candidates, project)
    request = _request_from_args(args)
    checkpoint_dir = Path(args.checkpoint_dir)
    checkpoint_dir.mkdir(parents=True, exist_ok=True)
    for path in checkpoint_dir.glob(f"{project.project_id}*.sqlite3*"):
        path.unlink()

    realtime_cp = checkpoint_dir / f"{project.project_id}.realtime.sqlite3"
    resumed_cp = checkpoint_dir / f"{project.project_id}.resumed.sqlite3"
    realtime = run_validation(
        project,
        candidate_set,
        request,
        checkpoint_path=realtime_cp,
        mode="realtime",
    )
    partial_request = replace(request, interrupt_after_commits=args.interrupt_after)
    partial = run_validation(
        project,
        candidate_set,
        partial_request,
        checkpoint_path=resumed_cp,
        mode="resume",
    )
    restored = run_validation(
        project,
        candidate_set,
        request,
        checkpoint_path=resumed_cp,
        mode="resume",
    )
    full = run_validation(project, candidate_set, request, mode="full")
    realtime_vs_full = compare_results(
        realtime, full, left_label="realtime", right_label="full"
    )
    resumed_vs_full = compare_results(restored, full, left_label="resumed", right_label="full")
    payload = {
        "ok": realtime_vs_full.equal and resumed_vs_full.equal,
        "project_id": project.project_id,
        "checkpoint_context": SnapshotContext.create(project, candidate_set, request).to_dict(),
        "partial": {
            "interrupted": partial.interrupted,
            "committed_combos": partial.schedule.committed_combos,
            "submitted_candidates": sum(
                1 for item in partial.candidates if item.state == "SUBMITTED"
            ),
        },
        "resume": {
            "restored_combos": restored.schedule.restored_combos,
            "simulated_combos": restored.schedule.simulated_combos,
        },
        "realtime_vs_full": realtime_vs_full.to_dict(),
        "resumed_vs_full": resumed_vs_full.to_dict(),
        "schedule": {
            "realtime": realtime.schedule.to_dict(),
            "resumed": restored.schedule.to_dict(),
            "full": full.schedule.to_dict(),
        },
    }
    if args.output:
        path = Path(args.output)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    if not payload["ok"]:
        raise BalanceError(
            ErrorCode.DIFF,
            "three-path comparison failed",
            project.project_id,
            "result",
            detail=payload,
        )
    _json_print(payload)
    return 0


def cmd_injections(args: argparse.Namespace) -> int:
    data_dir = Path(args.data)
    specs = json.loads((data_dir / "injection_cases.json").read_text(encoding="utf-8"))
    results: list[dict[str, Any]] = []
    ok = True
    for case in specs["project_cases"]:
        raw = json.loads((data_dir / case["project"]).read_text(encoding="utf-8"))
        mutated = apply_project_injection(raw, case["operation"])
        try:
            parse_project(mutated)
        except BalanceError as exc:
            matched = (
                exc.code.value == case["expected_code"]
                and exc.object_id == case["expected_object_id"]
                and exc.field == case["expected_field"]
            )
            ok = ok and matched
            results.append(
                {
                    "id": case["id"],
                    "expected": {
                        "code": case["expected_code"],
                        "object_id": case["expected_object_id"],
                        "field": case["expected_field"],
                    },
                    "actual": exc.to_dict(),
                    "passed": matched,
                }
            )
        else:
            ok = False
            results.append(
                {
                    "id": case["id"],
                    "expected": case,
                    "actual": None,
                    "passed": False,
                }
            )
    payload = {"ok": ok, "cases": results}
    if args.output:
        path = Path(args.output)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
    _json_print(payload)
    return 0 if ok else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="balance-lab", description="Battle Balance Lab CLI")
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate deterministic sample projects")
    generate.add_argument("--output", default="data/samples")
    generate.set_defaults(func=cmd_generate)

    validate = subparsers.add_parser("validate", help="validate a project and candidate set")
    validate.add_argument("--project", required=True)
    validate.add_argument("--candidates")
    validate.set_defaults(func=cmd_validate)

    run = subparsers.add_parser("run", help="run a validation")
    run.add_argument("--project", required=True)
    run.add_argument("--candidates", required=True)
    run.add_argument("--mode", choices=["realtime", "resume", "full"], default="realtime")
    run.add_argument("--checkpoint")
    run.add_argument("--target-samples", type=int, default=64)
    run.add_argument("--min-samples", type=int, default=2)
    run.add_argument("--total-sample-budget", type=int, default=1_200)
    run.add_argument("--parallel-slots", type=int)
    run.add_argument("--output")
    run.add_argument("--markdown")
    run.set_defaults(func=cmd_run)

    three = subparsers.add_parser("three-path", help="run and compare realtime/resume/full")
    three.add_argument("--project", required=True)
    three.add_argument("--candidates", required=True)
    three.add_argument("--checkpoint-dir", default="artifacts/checkpoints")
    three.add_argument("--target-samples", type=int, default=64)
    three.add_argument("--min-samples", type=int, default=2)
    three.add_argument("--total-sample-budget", type=int, default=1_200)
    three.add_argument("--interrupt-after", type=int, default=12)
    three.add_argument("--output")
    three.set_defaults(func=cmd_three_path)

    injections = subparsers.add_parser("injections", help="exercise project error-code injections")
    injections.add_argument("--data", default="data/samples")
    injections.add_argument("--output")
    injections.set_defaults(func=cmd_injections)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except BalanceError as exc:
        _json_print({"ok": False, "error": exc.to_dict()})
        return 2
    except (OSError, json.JSONDecodeError) as exc:
        _json_print(
            {
                "ok": False,
                "error": {
                    "code": ErrorCode.SCHEMA.value,
                    "message": str(exc),
                    "object_id": "",
                    "field": "",
                },
            }
        )
        return 2


if __name__ == "__main__":
    sys.exit(main())
