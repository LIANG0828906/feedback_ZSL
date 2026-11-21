from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from .archive import ArchiveService
from .config import load_config
from .errors import E_VERSION_LOSS, AppError
from .generator import generate_bundle
from .metadata import derive_boundaries, load_metadata
from .publish import FileVersionPublisher
from .review import review_versions
from .util import atomic_write_pretty_json
from .versions import downgrade_document, upgrade_to_v3
from .wav import read_wav


def _print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))


def _load_inputs(args: argparse.Namespace):
    wav = read_wav(args.wav)
    metadata = load_metadata(args.metadata, wav=wav)
    return wav, metadata


def cmd_generate(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    baseline = generate_bundle(args.out, config, seed=args.seed)
    if args.summary:
        _print_json(
            {
                "ok": True,
                "out": str(Path(args.out)),
                "seed": baseline["seed"],
                "asset_count": len(baseline["assets"]),
                "recovery_demo": baseline["recovery_demo"],
                "published_version": baseline["published_version"],
                "trace": str(Path(args.out) / "trace.jsonl"),
            }
        )
    return 0


def cmd_inspect(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    wav, metadata = _load_inputs(args)
    upgrade = upgrade_to_v3(metadata, wav, config)
    derived = derive_boundaries(upgrade.document, config)
    result = {
        "ok": True,
        "operation": "inspect",
        "wav": wav.summary(),
        "parsed_metadata": metadata.summary(),
        "internal_version": 3,
        "boundary_source": derived.source,
        "boundary_rule": derived.rule,
        "boundaries": [
            {"index": index, "start_sample": start, "end_sample": end}
            for index, (start, end) in enumerate(derived.segments)
        ],
        "upgrade": upgrade.to_dict(),
    }
    _print_json(result)
    return 0


def cmd_upgrade(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    wav, metadata = _load_inputs(args)
    result = upgrade_to_v3(metadata, wav, config)
    if args.out:
        atomic_write_pretty_json(args.out, result.payload)
    _print_json(result.to_dict())
    return 0


def cmd_downgrade(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    wav, metadata = _load_inputs(args)
    upgraded = upgrade_to_v3(metadata, wav, config)
    target = int(args.target.lower().lstrip("v"))
    try:
        result = downgrade_document(
            upgraded.document,
            target,
            wav,
            config,
            allow_loss=args.allow_loss,
        )
    except AppError as exc:
        if exc.code == E_VERSION_LOSS and args.out and args.allow_loss:
            atomic_write_pretty_json(args.out, {"error": exc.to_dict()})
        raise
    if args.out:
        atomic_write_pretty_json(args.out, result.payload)
    _print_json(result.to_dict())
    return 0


def cmd_review(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    wav = read_wav(args.wav)
    documents = {}
    for version, path in ((1, args.v1), (2, args.v2), (3, args.v3)):
        if path:
            documents[version] = load_metadata(path, wav=wav)
    if len(documents) < 2:
        raise AppError(
            "E_USAGE",
            "review requires at least two of --v1, --v2 and --v3",
            field="review",
        )
    report = review_versions(wav, documents, config)
    if args.out:
        atomic_write_pretty_json(args.out, report)
    _print_json(report)
    return 0 if report["ok"] else 1


def _publish_result(
    workspace: str | Path,
    result,
    *,
    print_result: bool = True,
) -> dict[str, Any]:
    publisher = FileVersionPublisher(workspace)
    published = publisher.publish(
        result.manifest,
        [analysis.to_dict() for analysis in result.analyses],
        target_version=result.target_version,
        loss_manifest=result.loss_manifest,
        field_mapping=list(result.field_mapping),
        expected_manifest_chain_head=result.manifest["chain_head"],
    )
    query = publisher.query(
        published["version_id"],
        expected_version_hash=published["content_hash"],
    )
    payload = {
        "ok": True,
        "archive": result.to_dict(),
        "published": published,
        "query": {
            "hit_version": query["hit_version"],
            "content_hash": query["content_hash"],
            "manifest_chain_head": query["manifest_chain_head"],
        },
    }
    if print_result:
        _print_json(payload)
    return payload


def cmd_archive(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    wav, metadata = _load_inputs(args)
    service = ArchiveService(args.workspace, config)
    crash_callback = None
    if args.crash_after is not None:
        crash_callback = lambda: os._exit(137)
    result = service.execute(
        wav,
        metadata,
        target_version=int(args.target.lower().lstrip("v")),
        parameters_version=args.parameters_version,
        crash_after_commits=args.crash_after,
        crash_callback=crash_callback,
    )
    if args.no_publish:
        _print_json(result.to_dict())
    else:
        _publish_result(args.workspace, result)
    return 0


def cmd_recover(args: argparse.Namespace) -> int:
    config = load_config(args.config)
    wav, metadata = _load_inputs(args)
    service = ArchiveService(args.workspace, config)
    result = service.execute(
        wav,
        metadata,
        target_version=int(args.target.lower().lstrip("v")),
        parameters_version=args.parameters_version,
    )
    if args.no_publish:
        _print_json(result.to_dict())
    else:
        _publish_result(args.workspace, result)
    return 0


def cmd_query(args: argparse.Namespace) -> int:
    publisher = FileVersionPublisher(args.workspace)
    result = publisher.query(
        args.version_id,
        expected_version_hash=args.expected_hash,
        verify_manifest=not args.no_verify_manifest,
    )
    _print_json(result)
    return 0


def cmd_gui(args: argparse.Namespace) -> int:
    try:
        from .gui import run_gui
    except ImportError as exc:
        raise AppError(
            "E_USAGE",
            "Tkinter is unavailable; install python3-tk or run via scripts/gui.sh",
            field="tkinter",
            details={"error": str(exc)},
        ) from exc

    config = load_config(args.config)
    run_gui(config, workspace=args.workspace)
    return 0


def cmd_selftest(args: argparse.Namespace) -> int:
    import unittest

    root = Path(__file__).resolve().parent.parent
    suite = unittest.defaultTestLoader.discover(str(root / "tests"))
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    return 0 if result.wasSuccessful() else 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="loudness-archive",
        description="Game-voice loudness metadata archive workbench",
    )
    parser.add_argument(
        "--config",
        default=None,
        help="workbench JSON configuration (default: config/workbench.json)",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    generate = sub.add_parser("generate", help="generate deterministic samples")
    generate.add_argument("--out", required=True)
    generate.add_argument("--seed", type=int, default=20261122)
    generate.add_argument("--summary", action="store_true")
    generate.set_defaults(func=cmd_generate)

    inspect = sub.add_parser("inspect", help="inspect WAV and one metadata file")
    inspect.add_argument("--wav", required=True)
    inspect.add_argument("--metadata", required=True)
    inspect.set_defaults(func=cmd_inspect)

    upgrade = sub.add_parser("upgrade", help="upgrade v1/v2 metadata to v3")
    upgrade.add_argument("--wav", required=True)
    upgrade.add_argument("--metadata", required=True)
    upgrade.add_argument("--out")
    upgrade.set_defaults(func=cmd_upgrade)

    downgrade = sub.add_parser("downgrade", help="downgrade v3 metadata")
    downgrade.add_argument("--wav", required=True)
    downgrade.add_argument("--metadata", required=True)
    downgrade.add_argument("--target", choices=("v1", "v2", "v3"), required=True)
    downgrade.add_argument("--allow-loss", action="store_true")
    downgrade.add_argument("--out")
    downgrade.set_defaults(func=cmd_downgrade)

    review = sub.add_parser("review", help="cross-version segment review")
    review.add_argument("--wav", required=True)
    review.add_argument("--v1")
    review.add_argument("--v2")
    review.add_argument("--v3")
    review.add_argument("--out")
    review.set_defaults(func=cmd_review)

    archive = sub.add_parser("archive", help="archive all segments atomically")
    archive.add_argument("--workspace", required=True)
    archive.add_argument("--wav", required=True)
    archive.add_argument("--metadata", required=True)
    archive.add_argument("--target", choices=("v1", "v2", "v3"), default="v3")
    archive.add_argument("--parameters-version")
    archive.add_argument("--crash-after", type=int)
    archive.add_argument("--no-publish", action="store_true")
    archive.set_defaults(func=cmd_archive)

    recover = sub.add_parser("recover", help="resume an interrupted archive")
    recover.add_argument("--workspace", required=True)
    recover.add_argument("--wav", required=True)
    recover.add_argument("--metadata", required=True)
    recover.add_argument("--target", choices=("v1", "v2", "v3"), default="v3")
    recover.add_argument("--parameters-version")
    recover.add_argument("--no-publish", action="store_true")
    recover.set_defaults(func=cmd_recover)

    query = sub.add_parser("query", help="query an immutable published version")
    query.add_argument("--workspace", required=True)
    query.add_argument("--version-id")
    query.add_argument("--expected-hash")
    query.add_argument("--no-verify-manifest", action="store_true")
    query.set_defaults(func=cmd_query)

    gui = sub.add_parser("gui", help="open the Tk workbench")
    gui.add_argument("--workspace", default="workbench_data")
    gui.set_defaults(func=cmd_gui)

    selftest = sub.add_parser("selftest", help="run the bundled unittest suite")
    selftest.set_defaults(func=cmd_selftest)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except AppError as exc:
        print(
            json.dumps(exc.to_dict(), ensure_ascii=False, indent=2, sort_keys=True),
            file=sys.stderr,
        )
        return 2
    except KeyboardInterrupt:
        print(
            json.dumps(
                {"ok": False, "code": "E_CANCELLED", "message": "interrupted"},
                ensure_ascii=False,
            ),
            file=sys.stderr,
        )
        return 130

