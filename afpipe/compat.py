"""Cross-version compatibility matrix and rejection verification."""

from __future__ import annotations

import os

from .config import INDEX_FORMAT_VERSION, LEGACY_INDEX_FORMAT_VERSION, Config
from .errors import VersionError
from .indexstore import (
    IndexStore,
    read_legacy_v1,
    upgrade_legacy_store,
    write_legacy_v1_store,
)
from .manifest import Manifest, load_manifest
from .pipeline import process_file
from .search import Searcher


def build_legacy_and_upgrade(work_dir: str, cfg: Config,
                             manifest: Manifest) -> dict:
    """Create a real 1.0.0 store, then explicitly upgrade it to 2.0.0."""
    legacy_dir = os.path.join(work_dir, "index_legacy_v1")
    upgraded_dir = os.path.join(work_dir, "index_upgraded_v2")
    records = []
    for media in manifest.index_files():
        processed = process_file(media, manifest, cfg)
        records.append((media, processed.track, processed.content_hash))
    write_legacy_v1_store(
        legacy_dir, cfg, manifest.fingerprint, records
    )
    read_legacy_v1(legacy_dir)  # sanity: it really is v1
    result = upgrade_legacy_store(
        legacy_dir, upgraded_dir, cfg,
        manifest_fingerprint=manifest.fingerprint,
    )
    return {
        "legacy_dir": legacy_dir,
        "upgraded_dir": upgraded_dir,
        "trail": result["trail"],
    }


def compatibility_matrix(manifest_path: str, current_index_dir: str,
                         upgraded_index_dir: str, legacy_index_dir: str,
                         cfg: Config) -> dict:
    """Run every query format against every index format version.

    Returns per-cell hit rates plus explicit rejection evidence.
    """
    manifest = load_manifest(manifest_path)
    query_media = manifest.query_files()
    stores = {
        INDEX_FORMAT_VERSION: current_index_dir,
        f"upgraded-{INDEX_FORMAT_VERSION}": upgraded_index_dir,
        LEGACY_INDEX_FORMAT_VERSION: legacy_index_dir,
    }
    matrix: list[dict] = []
    for store_label, store_dir in stores.items():
        for format_kind in ("pcm", "lossy"):
            cell = {
                "index_format": store_label,
                "query_format": format_kind,
                "queries": 0,
                "hits": 0,
                "hit_rate": None,
                "status": "ok",
                "rejection": None,
            }
            try:
                if store_label == LEGACY_INDEX_FORMAT_VERSION:
                    # Loading must be refused with E_VER, never silently used.
                    IndexStore(store_dir).load(
                        cfg,
                        manifest_fingerprint=manifest.fingerprint,
                    )
                    cell["status"] = "UNEXPECTED_ACCEPT"
                else:
                    store = IndexStore(store_dir).load(
                        cfg,
                        manifest_fingerprint=manifest.fingerprint,
                    )
                    searcher = Searcher(store, cfg, manifest)
                    for media in query_media:
                        if media.format_kind != format_kind:
                            continue
                        qid = media.file_id.rsplit("_", 1)[0]
                        result = searcher.query(media, query_id=qid)
                        expected_group = manifest.queries_meta[qid]["group_id"]
                        cell["queries"] += 1
                        if result.families and \
                                result.families[0].group_id == expected_group:
                            cell["hits"] += 1
                    if cell["queries"]:
                        cell["hit_rate"] = round(
                            cell["hits"] / cell["queries"], 6)
            except VersionError as exc:
                cell["status"] = "rejected"
                cell["rejection"] = exc.to_dict()
            matrix.append(cell)

    # Parameter-version mismatch rejection (same layout, changed parameters).
    mismatch_cell = {
        "index_format": INDEX_FORMAT_VERSION,
        "query_format": "parameter-mismatch",
        "status": "ok",
        "rejection": None,
    }
    altered = cfg.with_overrides(quant_bits=4)
    try:
        IndexStore(current_index_dir).load(
            altered, manifest_fingerprint=manifest.fingerprint
        )
        mismatch_cell["status"] = "UNEXPECTED_ACCEPT"
    except VersionError as exc:
        mismatch_cell["status"] = "rejected"
        mismatch_cell["rejection"] = exc.to_dict()
    matrix.append(mismatch_cell)

    accepted = [c for c in matrix if c["status"] == "ok"]
    return {
        "parameter_version": cfg.parameter_version(),
        "summary": {
            "cells": len(matrix),
            "accepted": len(accepted),
            "rejected": sum(1 for c in matrix if c["status"] == "rejected"),
            "unexpected": sum(
                1 for c in matrix if c["status"] == "UNEXPECTED_ACCEPT"),
        },
        "matrix": matrix,
    }


def cross_version_family_duplication_check(manifest_path: str,
                                           mixed_index_dir: str,
                                           cfg: Config) -> dict:
    """A coexistence store must not double count one physical segment."""
    manifest = load_manifest(manifest_path)
    store = IndexStore(mixed_index_dir).load(
        cfg, manifest_fingerprint=manifest.fingerprint
    )
    searcher = Searcher(store, cfg, manifest)
    rows = []
    for media in manifest.query_files():
        if media.format_kind != "lossy":
            continue
        qid = media.file_id.rsplit("_", 1)[0]
        result = searcher.query(media, query_id=qid)
        top = result.families[0] if result.families else None
        rows.append({
            "query_id": qid,
            "top_physical_key": top.physical_key if top else None,
            "family_count": len(result.families),
            "member_file_ids": [m.file_id for m in top.members] if top else [],
            "lineages": sorted({
                store.entry_by_file(m.file_id).lineage
                for m in top.members
            }) if top else [],
            "duplicate_physical_keys": (
                len([m for m in result.matches
                     if top and m.physical_key == top.physical_key])
                - len(top.members) if top else 0
            ),
        })
    return {"rows": rows}


def quantisation_monotonicity(manifest_path: str, cfg: Config) -> dict:
    """Observe match rate as quantisation tightens (1 -> 2 -> 4 bits)."""
    manifest = load_manifest(manifest_path)
    levels = (1, 2, 4)
    report: dict[str, list] = {"pairs": []}
    for group_id in sorted(manifest.groups):
        files = [f for f in manifest.index_files()
                 if f.group_id == group_id]
        pcm = next(f for f in files if f.format_kind == "pcm")
        lossy = next((f for f in files if f.format_kind == "lossy"), None)
        if lossy is None:
            continue
        row = {"group_id": group_id, "rates": {}, "exact_rates": {}}
        tracks = {}
        for bits in levels:
            level_cfg = cfg.with_overrides(quant_bits=bits)
            for media in (pcm, lossy):
                processed = process_file(media, manifest, level_cfg)
                tracks[(bits, media.format_kind)] = processed.track
        from .fingerprint import frame_match_rate, hamming_match_rate

        for bits in levels:
            row["exact_rates"][str(bits)] = round(frame_match_rate(
                tracks[(bits, "pcm")], tracks[(bits, "lossy")]), 6)
            row["rates"][str(bits)] = round(hamming_match_rate(
                tracks[(bits, "pcm")], tracks[(bits, "lossy")],
                cfg.with_overrides(quant_bits=bits)), 6)
        rates = [row["rates"][str(b)] for b in levels]
        row["monotonic_non_increasing"] = all(
            rates[i] >= rates[i + 1] for i in range(len(rates) - 1)
        )
        row["meets_floor_at_default"] = (
            row["rates"][str(cfg.quant_bits)] >= cfg.match_rate_floor
        )
        report["pairs"].append(row)
    return report
