from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path
from typing import Any

from .errors import E_PARAM, WorkbenchError
from .injection import InjectionConfig
from .jsonutil import canonical_json_text
from .metadata import export_metadata, project_model
from .package import build_package, read_artifact_model, write_package
from .pipeline import discover_sample_groups, load_sample, truth_path
from .samplegen import generate_samples
from .segmented import pack_segmented
from .selfcheck import run_selfcheck
from .validator import validate_artifact


def _json(value: Any, pretty: bool = True) -> str:
    if pretty:
        return json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True)
    return canonical_json_text(value)


def _error_json(exc: WorkbenchError) -> str:
    return _json({"ok": False, "error": exc.to_dict()})


def _load(args: argparse.Namespace):
    injection = InjectionConfig.from_file(args.injection) if args.injection else None
    if injection is not None and getattr(args, "segmented", False) and injection.corrupt_track:
        injection = None
    return load_sample(
        args.sample,
        args.source_version,
        injection=injection,
    )


def command_generate(args: argparse.Namespace) -> int:
    index = generate_samples(args.out, seed=args.seed, clean=not args.keep)
    print(_json({"ok": True, "index": index}))
    return 0


def command_selfcheck(args: argparse.Namespace) -> int:
    report = run_selfcheck(args.samples, checkpoint_root=args.checkpoint)
    print(_json(report))
    return 0 if report["ok"] else 1


def command_inspect(args: argparse.Namespace) -> int:
    loaded = _load(args)
    model = loaded.model
    print(
        _json(
            {
                "ok": True,
                "sample": str(loaded.sample_dir),
                "source_version": model.source_version,
                "model_id": model.model_id,
                "title": model.title,
                "timebase": model.timebase.to_dict(),
                "duration_ticks": model.duration_ticks,
                "tracks": [track.declaration() for track in model.tracks],
                "chapters": [chapter.declaration() for chapter in model.chapters],
                "tags": model.tags,
                "cover_sha256": hashlib.sha256(model.cover).hexdigest(),
                "truth_fingerprint": loaded.truth.fingerprint if loaded.truth else None,
            }
        )
    )
    return 0


def command_pack(args: argparse.Namespace) -> int:
    loaded = _load(args)
    injection = InjectionConfig.from_file(args.injection) if args.injection else None
    artifact_path = Path(args.out)
    artifact_path.parent.mkdir(parents=True, exist_ok=True)
    if args.segmented:
        result = pack_segmented(
            loaded.model,
            args.target,
            args.checkpoint,
            workers=args.workers,
            per_thread_buffer_limit=args.buffer,
            parameter_version=args.parameter_version,
            injection=injection,
            artifact_path=artifact_path,
            truth_path=truth_path(loaded.sample_dir) if loaded.truth else None,
        )
        print(
            _json(
                {
                    "ok": result.ok,
                    "artifact": str(artifact_path) if result.ok else None,
                    "result": result.to_dict(),
                }
            )
        )
        return 0 if result.ok else 1
    package = build_package(loaded.model, args.target)
    write_package(package, artifact_path)
    validation = validate_artifact(artifact_path, truth_path(loaded.sample_dir))
    print(
        _json(
            {
                "ok": validation.ok,
                "artifact": str(artifact_path),
                "artifact_sha256": package.artifact_sha256,
                "warnings": package.warning_dicts(),
                "mapping_trace": list(package.trace),
                "validation": validation.to_dict(),
            }
        )
    )
    return 0 if validation.ok else 1


def command_validate(args: argparse.Namespace) -> int:
    result = validate_artifact(args.artifact, args.truth)
    print(_json(result.to_dict()))
    return 0 if result.ok else 1


def command_roundtrip(args: argparse.Namespace) -> int:
    loaded = _load(args)
    target = args.target
    exported = export_metadata(loaded.model, target)
    from .metadata import load_metadata_dict

    reimported = load_metadata_dict(exported.metadata)
    projected = project_model(loaded.model, target)
    comparisons = []
    expressible_equal = True
    for field_name, left, right in (
        ("model_id", projected.model_id, reimported.model_id),
        ("title", projected.title, reimported.title),
        ("duration_ticks", projected.duration_ticks, reimported.duration_ticks),
        ("timebase", projected.timebase.to_dict(), reimported.timebase.to_dict()),
        ("tags", projected.tags, reimported.tags),
        ("cover", projected.cover, reimported.cover),
        ("editorial", projected.editorial, reimported.editorial),
    ):
        ok = left == right
        expressible_equal = expressible_equal and ok
        comparisons.append({"field": field_name, "ok": ok, "left": str(left), "right": str(right)})
    left_tracks = {
        track.track_id: (
            track.language,
            track.timebase.to_dict(),
            track.sample_count,
            track.duration_ticks,
            track.roles,
            track.hdr,
            track.custom,
        )
        for track in projected.tracks
    }
    right_tracks = {
        track.track_id: (
            track.language,
            track.timebase.to_dict(),
            track.sample_count,
            track.duration_ticks,
            track.roles,
            track.hdr,
            track.custom,
        )
        for track in reimported.tracks
    }
    expressible_equal = expressible_equal and left_tracks == right_tracks
    left_chapters = [
        (chapter.chapter_id, chapter.start_ticks, chapter.end_ticks, chapter.title, chapter.tags)
        for chapter in projected.chapters
    ]
    right_chapters = [
        (chapter.chapter_id, chapter.start_ticks, chapter.end_ticks, chapter.title, chapter.tags)
        for chapter in reimported.chapters
    ]
    expressible_equal = expressible_equal and left_chapters == right_chapters
    print(
        _json(
            {
                "ok": expressible_equal,
                "source_version": loaded.metadata_version,
                "target_version": target,
                "warnings": exported.warning_dicts(),
                "comparisons": comparisons,
                "tracks_equal": left_tracks == right_tracks,
                "chapters_equal": left_chapters == right_chapters,
            }
        )
    )
    return 0 if expressible_equal else 1


def command_readback(args: argparse.Namespace) -> int:
    model = read_artifact_model(args.artifact)
    print(
        _json(
            {
                "ok": True,
                "source_version": model.source_version,
                "model_id": model.model_id,
                "duration_ticks": model.duration_ticks,
                "track_count": len(model.tracks),
                "track_sample_counts": {
                    track.track_id: {
                        "sample_count": track.sample_count,
                        "timebase": track.timebase.to_dict(),
                    }
                    for track in model.tracks
                },
                "chapters": [chapter.declaration() for chapter in model.chapters],
                "tags": model.tags,
            }
        )
    )
    return 0


def command_gui(args: argparse.Namespace) -> int:
    from .gui.main_window import run

    return run()


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mediaforge",
        description="Deterministic multimedia container workbench",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate-samples", help="generate deterministic local samples")
    generate.add_argument("--out", default="samples")
    generate.add_argument("--seed", type=int, default=20261123)
    generate.add_argument("--keep", action="store_true")
    generate.set_defaults(func=command_generate)

    selfcheck = subparsers.add_parser("selfcheck", help="run environment and sample self-check")
    selfcheck.add_argument("--samples", default="samples")
    selfcheck.add_argument("--checkpoint", default="artifacts/selfcheck-checkpoints")
    selfcheck.set_defaults(func=command_selfcheck)

    inspect_parser = subparsers.add_parser("inspect", help="load a sample and print the unified model")
    inspect_parser.add_argument("--sample", required=True)
    inspect_parser.add_argument("--source-version", default="v3")
    inspect_parser.add_argument("--injection")
    inspect_parser.set_defaults(func=command_inspect)

    pack = subparsers.add_parser("pack", help="export a package")
    pack.add_argument("--sample", required=True)
    pack.add_argument("--source-version", default="v3")
    pack.add_argument("--target", default="v3")
    pack.add_argument("--out", required=True)
    pack.add_argument("--injection")
    pack.add_argument("--segmented", action="store_true")
    pack.add_argument("--checkpoint", default="artifacts/checkpoints")
    pack.add_argument("--workers", type=int, default=2)
    pack.add_argument("--buffer", type=int, default=4 * 1024 * 1024)
    pack.add_argument("--parameter-version", default="params-default")
    pack.set_defaults(func=command_pack)

    validate = subparsers.add_parser("validate", help="independently validate a package")
    validate.add_argument("--artifact", required=True)
    validate.add_argument("--truth", required=True)
    validate.set_defaults(func=command_validate)

    roundtrip = subparsers.add_parser("roundtrip", help="check lossy metadata round-trip equivalence")
    roundtrip.add_argument("--sample", required=True)
    roundtrip.add_argument("--source-version", default="v3")
    roundtrip.add_argument("--target", default="v1")
    roundtrip.add_argument("--injection")
    roundtrip.set_defaults(func=command_roundtrip)

    readback = subparsers.add_parser("readback", help="read a unified model from a persisted package")
    readback.add_argument("--artifact", required=True)
    readback.set_defaults(func=command_readback)

    gui = subparsers.add_parser("gui", help="start the desktop workbench")
    gui.set_defaults(func=command_gui)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except WorkbenchError as exc:
        print(_error_json(exc), file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print(_json({"ok": False, "error": {"code": "E_INTERRUPTED", "message": "interrupted"}}), file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
