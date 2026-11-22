"""Command line entry points for the whole pipeline."""

from __future__ import annotations

import argparse
import json
import os
import sys

from .compat import (
    build_legacy_and_upgrade,
    compatibility_matrix,
    cross_version_family_duplication_check,
    quantisation_monotonicity,
)
from .config import Config
from .errors import AFPipeError
from .hashing import atomic_write_bytes, canonical_bytes
from .indexstore import IndexStore
from .manifest import load_manifest
from .samples import generate
from .builder import IndexBuildManager
from .search import Searcher
from .verify import verify_against_baseline, verify_against_ground_truth


def _config_from_args(args: argparse.Namespace) -> Config:
    overrides = {
        k: v for k, v in vars(args).items()
        if k in Config.__dataclass_fields__ and v is not None
    }
    return Config(**overrides).validate()


def _emit(payload, path: str | None) -> None:
    text = json.dumps(payload, indent=2, ensure_ascii=False, sort_keys=True)
    if path:
        atomic_write_bytes(path, text.encode("utf-8"))
    print(text)


def cmd_generate(args) -> int:
    manifest = generate(args.out, seed=args.seed)
    _emit({"manifest_path": os.path.join(args.out, "manifest.json"),
           "groups": len(manifest["groups"]),
           "queries": len(manifest["queries"]),
           "files": sum(len(g["files"]) for g in manifest["groups"].values())
                    + sum(len(q["files"]) for q in manifest["queries"].values())},
          args.report)
    return 0


def cmd_build(args) -> int:
    cfg = _config_from_args(args)
    manifest = load_manifest(args.manifest)
    manager = IndexBuildManager(args.work, args.index, cfg, manifest)
    report = manager.build(role=args.role, retries=args.retries)
    out = report.to_dict()
    out["index_identity"] = IndexStore(args.index).load(
        cfg, manifest_fingerprint=manifest.fingerprint
    ).identity()
    _emit(out, args.report)
    return 0 if not report.failed_files else 2


def cmd_search(args) -> int:
    cfg = _config_from_args(args)
    manifest = load_manifest(args.manifest)
    store = IndexStore(args.index).load(
        cfg, manifest_fingerprint=manifest.fingerprint
    )
    searcher = Searcher(store, cfg, manifest)
    if args.query_file_id:
        media = manifest.file(args.query_file_id)
        qid = media.file_id.rsplit("_", 1)[0]
        results = [searcher.query(media, query_id=qid).to_dict()]
    else:
        results = []
        for qid, query in sorted(manifest.queries_meta.items()):
            owned = [m for m in manifest.query_files()
                     if m.file_id in (f["file_id"] for f in query["files"])]
            media = next((m for m in owned if m.format_kind == "lossy"),
                         owned[0])
            results.append(searcher.query(media, query_id=qid).to_dict())
    _emit({"parameter_version": cfg.parameter_version(), "results": results},
          args.report)
    return 0


def cmd_verify(args) -> int:
    cfg = _config_from_args(args)
    if args.baseline:
        report = verify_against_baseline(
            args.manifest, args.index, args.baseline, cfg)
    else:
        report = verify_against_ground_truth(args.manifest, args.index, cfg)
    _emit(report.to_dict(), args.report)
    return 0 if report.ok else 3


def cmd_compat(args) -> int:
    cfg = _config_from_args(args)
    manifest = load_manifest(args.manifest)
    upgrade = build_legacy_and_upgrade(args.work, cfg, manifest)
    matrix = compatibility_matrix(
        args.manifest, args.index, upgrade["upgraded_dir"],
        upgrade["legacy_dir"], cfg,
    )
    # Coexistence store: native v2 entries + upgraded legacy entries together.
    mixed_dir = os.path.join(args.work, "index_mixed_v2")
    _build_mixed_store(args, cfg, manifest, mixed_dir, upgrade["legacy_dir"])
    duplication = cross_version_family_duplication_check(
        args.manifest, mixed_dir, cfg
    )
    monotonic = quantisation_monotonicity(args.manifest, cfg)
    payload = {
        "upgrade_trail": upgrade["trail"],
        "compatibility_matrix": matrix,
        "coexistence_dedup": duplication,
        "quantisation_monotonicity": monotonic,
    }
    _emit(payload, args.report)
    unexpected = matrix["summary"]["unexpected"]
    return 0 if unexpected == 0 else 4


def _build_mixed_store(args, cfg, manifest, mixed_dir, legacy_dir) -> None:
    """Native current entries plus upgraded legacy entries in one v2 store."""
    from .indexstore import IndexBuilder, read_legacy_v1, entry_id_for
    from .fingerprint import FrameFingerprint, FingerprintTrack
    from .manifest import MediaFile

    legacy = read_legacy_v1(legacy_dir)
    builder = IndexBuilder(mixed_dir, cfg, manifest.fingerprint)
    seen_files: set[str] = set()
    # First half of the physical material comes from the upgraded lineage.
    legacy_records = {r["id"]: r for r in legacy["records"]}
    for media in manifest.index_files():
        rec = legacy_records.get(media.file_id)
        if rec is None:
            continue
        frames = [FrameFingerprint(r["frame"], r["time"], r["hash"])
                  for r in rec["frames"]]
        builder.add_track(
            media,
            FingerprintTrack(media.file_id, frames, 0, media.duration_s),
            media.sha256,
            lineage="upgraded-from-1.0.0",
            entry_id=entry_id_for(media.file_id, media.sha256),
        )
        seen_files.add(media.file_id)
    # Remaining entries are freshly built (native lineage).
    from .pipeline import process_file

    for media in manifest.index_files():
        if media.file_id in seen_files:
            continue
        processed = process_file(media, manifest, cfg)
        builder.add_track(media, processed.track, processed.content_hash)
    builder.commit()


def cmd_checkpoint_info(args) -> int:
    from .checkpoint import CheckpointStore

    cfg = _config_from_args(args)
    manifest = load_manifest(args.manifest)
    store = CheckpointStore(args.work, cfg, manifest.fingerprint)
    reusable = store.load()
    _emit({"reusable_files": sorted(reusable),
           "snapshot": store.snapshot_summary(),
           "invalidation": store.invalidation_record()}, args.report)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="afpipe",
        description="Audio fingerprint and similar-segment retrieval pipeline",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("generate", help="generate deterministic samples")
    p.add_argument("--out", required=True)
    p.add_argument("--seed", type=int, default=20261123)
    p.add_argument("--report")
    p.set_defaults(func=cmd_generate)

    p = sub.add_parser("build", help="build fingerprint index")
    _add_common_config(p)
    p.add_argument("--manifest", required=True)
    p.add_argument("--work", required=True)
    p.add_argument("--index", required=True)
    p.add_argument("--role", default="index", choices=("index", "query"))
    p.add_argument("--retries", type=int, default=1)
    p.add_argument("--report")
    p.set_defaults(func=cmd_build)

    p = sub.add_parser("search", help="run similarity queries")
    _add_common_config(p)
    p.add_argument("--manifest", required=True)
    p.add_argument("--index", required=True)
    p.add_argument("--query-file-id")
    p.add_argument("--report")
    p.set_defaults(func=cmd_search)

    p = sub.add_parser("verify", help="verify persisted results")
    _add_common_config(p)
    p.add_argument("--manifest", required=True)
    p.add_argument("--index", required=True)
    p.add_argument("--baseline")
    p.add_argument("--report")
    p.set_defaults(func=cmd_verify)

    p = sub.add_parser("compat", help="compatibility matrix and monotonicity")
    _add_common_config(p)
    p.add_argument("--manifest", required=True)
    p.add_argument("--work", required=True)
    p.add_argument("--index", required=True)
    p.add_argument("--report")
    p.set_defaults(func=cmd_compat)

    p = sub.add_parser("checkpoint-info", help="inspect checkpoint state")
    _add_common_config(p)
    p.add_argument("--manifest", required=True)
    p.add_argument("--work", required=True)
    p.add_argument("--report")
    p.set_defaults(func=cmd_checkpoint_info)
    return parser


def _add_common_config(p: argparse.ArgumentParser) -> None:
    p.add_argument("--sample-rate", dest="sample_rate", type=int)
    p.add_argument("--channel-mode", dest="channel_mode",
                   choices=("mono", "keep"))
    p.add_argument("--max-channels", dest="max_channels", type=int)
    p.add_argument("--quant-bits", dest="quant_bits", type=int,
                   choices=(1, 2, 4))
    p.add_argument("--bands", type=int)
    p.add_argument("--frame-ms", dest="frame_ms", type=float)
    p.add_argument("--hop-ms", dest="hop_ms", type=float)
    p.add_argument("--max-workers", dest="max_workers", type=int)
    p.add_argument("--shard-frames", dest="shard_frames", type=int)
    p.add_argument("--match-rate-floor", dest="match_rate_floor", type=float)
    p.add_argument("--min-query-ms", dest="min_query_ms", type=float)
    p.add_argument("--max-query-ms", dest="max_query_ms", type=float)


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except AFPipeError as exc:
        payload = exc.to_dict()
        print(json.dumps(payload, indent=2, ensure_ascii=False),
              file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
