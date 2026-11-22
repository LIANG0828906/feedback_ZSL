"""Offline command-line entry point."""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

from .engine import FormationEngine
from .errors import ErrorCode, WorkbenchError
from .generator import generate_scene
from .models import SearchConfig
from .search import plan_member
from .validation import load_scene, validate_scene


def _write_json(payload: Any) -> None:
    print(json.dumps(payload, indent=2, ensure_ascii=False, default=str))


def _report_engine(engine: FormationEngine) -> dict[str, Any]:
    states = engine.member_states()
    return {
        "revision": engine.revision,
        "map_hash": engine.scene.map.content_hash(),
        "states": states,
        "manifest": (
            engine.last_manifest.to_dict() if engine.last_manifest is not None else None
        ),
    }


def command_generate(args: argparse.Namespace) -> int:
    payload = generate_scene(args.seed)
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(
        json.dumps(payload, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    _write_json(
        {
            "status": "ok",
            "output": str(output),
            "seed": args.seed,
            "members": [member["member_id"] for member in payload["members"]],
        }
    )
    return 0


def command_diagnose(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    engine = FormationEngine(scene, checkpoint_path=args.checkpoint)
    restore = engine.try_restore_checkpoint()
    if restore.restored:
        initial = {"restored": True, "restore": restore.to_dict()}
    else:
        initial = {
            "restored": False,
            "restore": restore.to_dict(),
            "build": engine.build_all(parallel=True, save_checkpoint=True),
        }
    edits: list[dict[str, Any]] = []
    for spec in args.edit or []:
        try:
            x_text, y_text, cost_text = spec.split(",")
            coord = (int(x_text), int(y_text))
            cost = int(cost_text)
        except ValueError as exc:
            raise WorkbenchError(
                ErrorCode.E_SCHEMA,
                f"invalid --edit '{spec}', expected x,y,cost",
                field="edit",
            ) from exc
        manifest = engine.apply_cell_cost(coord, cost)
        edits.append({"coord": list(coord), "cost": cost, "manifest": manifest.to_dict()})
    for spec in args.blockade or []:
        if ":" not in spec:
            raise WorkbenchError(
                ErrorCode.E_SCHEMA,
                f"invalid --blockade '{spec}', expected id:on|off",
                field="blockade",
            )
        blockade_id, state = spec.split(":", 1)
        manifest = engine.apply_blockade(blockade_id, state.lower() in {"on", "true", "1"})
        edits.append(
            {
                "blockade_id": blockade_id,
                "active": state,
                "manifest": manifest.to_dict(),
            }
        )

    comparison = None
    if args.compare:
        comparison = engine.compare_with_full(parallel=True).to_dict()
    if args.export:
        engine.export_json(args.export)
    payload = {
        "status": "ok",
        "scene": {
            "name": scene.name,
            "map_hash": scene.map.content_hash(),
            "initial": initial,
        },
        "edits": edits,
        "final": _report_engine(engine),
        "comparison": comparison,
        "export": str(args.export) if args.export else None,
    }
    _write_json(payload)
    return 0


def command_heuristic(args: argparse.Namespace) -> int:
    scene = load_scene(args.scene)
    config = SearchConfig(heuristic_weight=int(round(args.weight * 1000)))
    weighted_scene = replace(scene, search=config)
    try:
        path = plan_member(weighted_scene, args.member)
    except WorkbenchError as exc:
        _write_json({"status": "error", **exc.to_dict()})
        return 2
    _write_json(
        {
            "status": "ok",
            "member_id": args.member,
            "cost": path.cost,
            "cells": len(path.cells),
        }
    )
    return 0


def command_gui(args: argparse.Namespace) -> int:
    from .gui.app import run_gui

    return run_gui(scene_path=args.scene, checkpoint_path=args.checkpoint)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="formation-workbench",
        description="Formation path planning, tick replay and incremental cache diagnostics.",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate_parser = subparsers.add_parser("generate", help="generate deterministic sample")
    generate_parser.add_argument("--seed", type=int, default=167)
    generate_parser.add_argument(
        "--output", default="samples/formation_scene.json"
    )
    generate_parser.set_defaults(func=command_generate)

    diagnose_parser = subparsers.add_parser(
        "diagnose", help="run build, edits and full-consistency comparison"
    )
    diagnose_parser.add_argument("--scene", default="samples/formation_scene.json")
    diagnose_parser.add_argument(
        "--checkpoint", default="artifacts/formation_checkpoint.json"
    )
    diagnose_parser.add_argument(
        "--edit",
        action="append",
        help="cell edit x,y,cost; may be repeated",
    )
    diagnose_parser.add_argument(
        "--blockade",
        action="append",
        help="blockade edit id:on|off; may be repeated",
    )
    diagnose_parser.add_argument("--compare", action="store_true")
    diagnose_parser.add_argument("--export")
    diagnose_parser.set_defaults(func=command_diagnose)

    heuristic_parser = subparsers.add_parser(
        "heuristic", help="check weighted heuristic admissibility for a member"
    )
    heuristic_parser.add_argument("--scene", default="samples/formation_scene.json")
    heuristic_parser.add_argument("--member", default="alpha")
    heuristic_parser.add_argument("--weight", type=float, default=1.0)
    heuristic_parser.set_defaults(func=command_heuristic)

    gui_parser = subparsers.add_parser("gui", help="start PySide6 diagnostic workbench")
    gui_parser.add_argument("--scene", default="samples/formation_scene.json")
    gui_parser.add_argument(
        "--checkpoint", default="artifacts/formation_checkpoint.json"
    )
    gui_parser.set_defaults(func=command_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        _write_json({"status": "error", **exc.to_dict()})
        return 2
    except KeyboardInterrupt:
        _write_json(
            {
                "status": "cancelled",
                "note": "Only the last fully persisted checkpoint will be available on restart.",
            }
        )
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
