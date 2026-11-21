"""Atomic versioned snapshots, hash chains, edit journals and recovery."""

from __future__ import annotations

import copy
import json
import os
import tempfile
from dataclasses import dataclass, replace
from fractions import Fraction
from pathlib import Path
from typing import Any, Callable

from .compliance import build_compliance_report, build_tracks, speaker_markers
from .errors import E_RECOVERY_INVALID, RhythmError
from .models import (
    ItemResult,
    SolveResult,
    SourceBundle,
    SourceItem,
    canonical_json,
    content_hash,
    fraction_text,
)
from .solver import ImpactPlan, RhythmSolver
from .timebase import FrameTable, parse_frame_table, parse_wav_info, round_fraction


SCHEMA_VERSION = 1
GENESIS_HASH = "0" * 64


def chain_hash(previous: str, payload_hash: str) -> str:
    return content_hash({"previous": previous, "payload": payload_hash})


def atomic_write_bytes(path: str | Path, data: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{target.name}.", suffix=".tmp", dir=target.parent)
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(temp_name, target)
        directory_fd = os.open(target.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except BaseException:
        try:
            os.unlink(temp_name)
        except OSError:
            pass
        raise


def atomic_write_json(path: str | Path, value: Any) -> None:
    text = json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2, allow_nan=False) + "\n"
    atomic_write_bytes(path, text.encode("utf-8"))


@dataclass
class Snapshot:
    version: int
    material_id: str
    payload: dict[str, Any]
    payload_hash: str
    prev_hash: str
    chain_hash: str
    history: list[dict[str, Any]]
    path: Path

    @property
    def result(self) -> SolveResult:
        return SolveResult.from_dict(self.payload["result"])

    @property
    def report(self) -> dict[str, Any]:
        return copy.deepcopy(self.payload["compliance"])

    @property
    def tracks(self) -> dict[str, Any]:
        return copy.deepcopy(self.payload["tracks"])

    @property
    def trace_log(self) -> list[dict[str, Any]]:
        return copy.deepcopy(self.payload.get("trace_log", []))

    def canonical(self) -> dict[str, Any]:
        return {
            "schema_version": SCHEMA_VERSION,
            "version": self.version,
            "prev_hash": self.prev_hash,
            "payload_hash": self.payload_hash,
            "chain_hash": self.chain_hash,
            "history": self.history,
            "payload": self.payload,
        }


class VersionStore:
    def __init__(self, snapshot_path: str | Path):
        self.path = Path(snapshot_path)
        self.journal_path = self.path.with_name(self.path.name + ".journal")
        self.trace_path = self.path.with_name(self.path.stem + ".trace.jsonl")
        self.speech_track_path = self.path.with_name(self.path.stem + ".speech.json")
        self.description_track_path = self.path.with_name(self.path.stem + ".description.json")
        self.compliance_path = self.path.with_name(self.path.stem + ".compliance.json")

    # ------------------------------------------------------------- persistence
    def load(self) -> Snapshot | None:
        if not self.path.exists():
            return None
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot JSON cannot be loaded",
                None,
                {"path": str(self.path), "error": str(exc)},
            ) from exc
        return self._validate_snapshot(raw)

    def _validate_snapshot(self, raw: Any) -> Snapshot:
        required = {
            "schema_version",
            "version",
            "prev_hash",
            "payload_hash",
            "chain_hash",
            "history",
            "payload",
        }
        if not isinstance(raw, dict) or required - set(raw):
            missing = sorted(required - set(raw)) if isinstance(raw, dict) else sorted(required)
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot is missing required fields",
                None,
                {"missing": missing},
            )
        if raw["schema_version"] != SCHEMA_VERSION:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot schema version is unsupported",
                None,
                {"schema_version": raw["schema_version"]},
            )
        payload = raw["payload"]
        payload_required = {
            "material_id",
            "version",
            "params_version",
            "source",
            "result",
            "compliance",
            "tracks",
            "speakers",
            "trace_log",
            "txn_id",
        }
        if not isinstance(payload, dict) or payload_required - set(payload):
            missing = (
                sorted(payload_required - set(payload))
                if isinstance(payload, dict)
                else sorted(payload_required)
            )
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot payload is missing required fields",
                None,
                {"missing": missing},
            )
        actual_payload_hash = content_hash(payload)
        if actual_payload_hash != raw["payload_hash"]:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot payload hash does not match",
                None,
                {"declared": raw["payload_hash"], "actual": actual_payload_hash},
            )
        if int(payload["version"]) != int(raw["version"]):
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot and payload versions disagree",
                None,
                {"snapshot_version": raw["version"], "payload_version": payload["version"]},
            )
        expected_chain = chain_hash(str(raw["prev_hash"]), str(raw["payload_hash"]))
        if expected_chain != raw["chain_hash"]:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot chain hash is invalid",
                None,
                {"declared": raw["chain_hash"], "actual": expected_chain},
            )
        history = raw["history"]
        if not isinstance(history, list) or not history:
            raise RhythmError(E_RECOVERY_INVALID, "snapshot history is empty or invalid", None)
        previous = GENESIS_HASH
        for expected_version, entry in enumerate(history, start=1):
            if not isinstance(entry, dict) or {
                "version",
                "prev_hash",
                "payload_hash",
                "chain_hash",
            } - set(entry):
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    "snapshot history entry is malformed",
                    None,
                    {"expected_version": expected_version},
                )
            if int(entry["version"]) != expected_version:
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    "snapshot history version is not contiguous",
                    None,
                    {"expected": expected_version, "actual": entry["version"]},
                )
            if entry["prev_hash"] != previous:
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    "snapshot history is broken",
                    None,
                    {"version": expected_version, "expected_prev": previous},
                )
            calculated = chain_hash(entry["prev_hash"], entry["payload_hash"])
            if entry["chain_hash"] != calculated:
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    "snapshot history entry hash is invalid",
                    None,
                    {"version": expected_version},
                )
            previous = entry["chain_hash"]
        last = history[-1]
        if int(last["version"]) != int(raw["version"]) or last["chain_hash"] != raw["chain_hash"]:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot does not match its hash-chain head",
                None,
                {"history_head": last["version"], "snapshot_version": raw["version"]},
            )
        self._validate_payload(payload)
        return Snapshot(
            version=int(raw["version"]),
            material_id=str(payload["material_id"]),
            payload=copy.deepcopy(payload),
            payload_hash=str(raw["payload_hash"]),
            prev_hash=str(raw["prev_hash"]),
            chain_hash=str(raw["chain_hash"]),
            history=copy.deepcopy(history),
            path=self.path,
        )

    def _validate_payload(self, payload: dict[str, Any]) -> None:
        try:
            result = SolveResult.from_dict(payload["result"])
        except (KeyError, TypeError, ValueError) as exc:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot result cannot be reconstructed",
                None,
                {"error": str(exc)},
            ) from exc
        expected_result_hash = content_hash(result.canonical(include_hash=False))
        if result.result_hash != expected_result_hash:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "snapshot result hash is invalid",
                None,
                {"declared": result.result_hash, "actual": expected_result_hash},
            )
        for item in result.items:
            expected_item_hash = content_hash(item.canonical(include_hash=False))
            if item.result_hash != expected_item_hash:
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    "sentence result hash is invalid",
                    item.item_id,
                    {"declared": item.result_hash, "actual": expected_item_hash},
                )
        report = payload["compliance"]
        if not isinstance(report, dict):
            raise RhythmError(E_RECOVERY_INVALID, "compliance report is malformed", None)
        expected_report_hash = content_hash(
            {key: value for key, value in report.items() if key != "report_hash"}
        )
        if report.get("report_hash") != expected_report_hash:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "compliance report hash is invalid",
                None,
                {"declared": report.get("report_hash"), "actual": expected_report_hash},
            )
        for track_name in ("speech", "description"):
            track = payload["tracks"].get(track_name)
            if not isinstance(track, dict) or track.get("result_hash") != result.result_hash:
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    "delivery track result hash disagrees with snapshot",
                    None,
                    {"track": track_name},
                )
        if int(report.get("version", -1)) != result.version:
            raise RhythmError(E_RECOVERY_INVALID, "report and result versions disagree", None)
        if report.get("result_hash") != result.result_hash:
            raise RhythmError(E_RECOVERY_INVALID, "report and result hashes disagree", None)

    def save_snapshot(
        self,
        *,
        version: int,
        bundle: SourceBundle,
        result: SolveResult,
        base: Snapshot | None,
        txn_id: str,
    ) -> Snapshot:
        if result.version != version:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "result and snapshot versions disagree before commit",
                None,
                {"result_version": result.version, "snapshot_version": version},
            )
        if result.source_hash != bundle.source_hash or result.material_id != bundle.material_id:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "result does not belong to the bundle being committed",
                None,
                {
                    "result_source_hash": result.source_hash,
                    "bundle_source_hash": bundle.source_hash,
                    "result_material_id": result.material_id,
                    "bundle_material_id": bundle.material_id,
                },
            )
        expected_version = (base.version + 1) if base else 1
        if version != expected_version:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "commit version is not the next hash-chain version",
                None,
                {"base_version": base.version if base else 0, "target_version": version},
            )
        report = build_compliance_report(bundle, result)
        speech_track, description_track = build_tracks(bundle, result)
        prev_hash = base.chain_hash if base else GENESIS_HASH
        trace_log = copy.deepcopy(base.trace_log) if base else []
        trace_log.extend(copy.deepcopy(result.trace_events))
        trace_log.append(
            {
                "event": "version_publish",
                "sequence": len(trace_log) + 1,
                "version": version,
                "sentence_id": result.affected_ids[0] if result.affected_ids else "*",
                "affected_count": len(result.affected_ids),
                "conflict_chain_length": len(
                    {part for record in result.conflicts for part in (record.left_id, record.right_id)}
                ),
                "content_hash": result.result_hash,
                "reused_count": len(result.reused_ids),
            }
        )
        payload = {
            "material_id": bundle.material_id,
            "version": version,
            "params_version": bundle.params_version,
            "source": bundle.canonical(),
            "result": result.canonical(),
            "compliance": report.canonical(),
            "tracks": {
                "speech": speech_track.canonical(),
                "description": description_track.canonical(),
            },
            "speakers": speaker_markers(result),
            "trace_log": trace_log,
            "txn_id": txn_id,
        }
        payload_hash = content_hash(payload)
        snapshot_hash = chain_hash(prev_hash, payload_hash)
        history = copy.deepcopy(base.history) if base else []
        history.append(
            {
                "version": version,
                "prev_hash": prev_hash,
                "payload_hash": payload_hash,
                "chain_hash": snapshot_hash,
                "txn_id": txn_id,
            }
        )
        raw = {
            "schema_version": SCHEMA_VERSION,
            "version": version,
            "prev_hash": prev_hash,
            "payload_hash": payload_hash,
            "chain_hash": snapshot_hash,
            "history": history,
            "payload": payload,
        }
        atomic_write_json(self.path, raw)
        self._write_delivery_artifacts(payload)
        snapshot = self._validate_snapshot(raw)
        return snapshot

    def _write_delivery_artifacts(self, payload: dict[str, Any]) -> None:
        atomic_write_json(self.speech_track_path, payload["tracks"]["speech"])
        atomic_write_json(self.description_track_path, payload["tracks"]["description"])
        atomic_write_json(self.compliance_path, payload["compliance"])
        lines = [
            canonical_json(event)
            for event in payload.get("trace_log", [])
        ]
        atomic_write_bytes(
            self.trace_path,
            ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8"),
        )

    # ----------------------------------------------------------------- journal
    def write_journal(self, journal: dict[str, Any]) -> None:
        body = {"schema_version": SCHEMA_VERSION, **journal}
        value = {"journal_hash": content_hash(body), **body}
        atomic_write_json(self.journal_path, value)

    def load_journal(self) -> dict[str, Any] | None:
        if not self.journal_path.exists():
            return None
        try:
            value = json.loads(self.journal_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "recovery journal cannot be loaded",
                None,
                {"path": str(self.journal_path), "error": str(exc)},
            ) from exc
        if not isinstance(value, dict):
            raise RhythmError(E_RECOVERY_INVALID, "recovery journal must be an object", None)
        declared = value.get("journal_hash")
        body = {key: item for key, item in value.items() if key != "journal_hash"}
        if declared != content_hash(body):
            raise RhythmError(
                E_RECOVERY_INVALID,
                "recovery journal hash is invalid",
                None,
                {"declared": declared, "actual": content_hash(body)},
            )
        required = {
            "schema_version",
            "txn_id",
            "base_version",
            "base_chain_hash",
            "material_id",
            "source_hash",
            "params_version",
            "source",
            "changed_id",
            "affected_ids",
            "reasons",
            "checkpoint",
            "new_version",
        }
        missing = sorted(required - set(body))
        if missing:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "recovery journal is missing fields",
                None,
                {"missing": missing},
            )
        if body["schema_version"] != SCHEMA_VERSION:
            raise RhythmError(E_RECOVERY_INVALID, "recovery journal schema is unsupported", None)
        return body

    def remove_journal(self) -> None:
        try:
            self.journal_path.unlink()
        except FileNotFoundError:
            return
        directory_fd = os.open(self.journal_path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)

    def sync_delivery_artifacts(self, snapshot: Snapshot) -> None:
        self._write_delivery_artifacts(snapshot.payload)


def bundle_from_canonical(source: dict[str, Any]) -> SourceBundle:
    items: list[SourceItem] = []
    for value in source["items"]:
        items.append(
            SourceItem(
                item_id=value["id"],
                kind=value["kind"],
                text=value["text"],
                start_sample=int(value["start_sample"]),
                end_sample=int(value["end_sample"]),
                speaker=value["speaker"],
                priority=int(value["priority"]),
                max_cps=Fraction(value["max_cps"]),
                source_rate_cps=Fraction(value["source_rate_cps"]),
                min_gap_samples=int(value.get("min_gap_samples", 0)),
            )
        )
    return SourceBundle(
        material_id=source["material_id"],
        sample_rate=int(source["sample_rate"]),
        frame_table_path=source["frame_table_path"],
        wav_path=source["wav_path"],
        items=tuple(items),
        params_version=source["params_version"],
        min_readable_ratio=Fraction(source.get("min_readable_ratio", "1")),
        max_iterations=int(source.get("max_iterations", 256)),
    )


def apply_item_patch(item: SourceItem, patch: dict[str, Any], sample_rate: int) -> SourceItem:
    allowed = {
        "text",
        "priority",
        "max_cps",
        "source_rate_cps",
        "speaker",
        "start_sample",
        "end_sample",
        "min_gap_samples",
        "min_gap_ms",
    }
    unknown = sorted(set(patch) - allowed)
    if unknown:
        raise RhythmError(
            E_RECOVERY_INVALID,
            "edit patch contains unsupported fields",
            item.item_id,
            {"unknown_fields": unknown},
        )
    changes: dict[str, Any] = {}
    for key in ("text", "speaker"):
        if key in patch:
            value = patch[key]
            if not isinstance(value, str) or not value.strip():
                raise RhythmError(E_RECOVERY_INVALID, f"{key} must be non-empty", item.item_id)
            changes[key] = value
    if "priority" in patch:
        value = patch["priority"]
        if isinstance(value, bool) or not isinstance(value, int):
            raise RhythmError(E_RECOVERY_INVALID, "priority must be an integer", item.item_id)
        changes["priority"] = value
    for key in ("max_cps", "source_rate_cps"):
        if key in patch:
            try:
                value = Fraction(str(patch[key]))
            except (ValueError, ZeroDivisionError) as exc:
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    f"{key} must be a positive number",
                    item.item_id,
                ) from exc
            if value <= 0:
                raise RhythmError(E_RECOVERY_INVALID, f"{key} must be positive", item.item_id)
            changes[key] = value
    for key in ("start_sample", "end_sample", "min_gap_samples"):
        if key in patch:
            value = patch[key]
            if isinstance(value, bool) or not isinstance(value, int):
                raise RhythmError(E_RECOVERY_INVALID, f"{key} must be integer", item.item_id)
            changes[key] = value
    if "min_gap_ms" in patch:
        value = patch["min_gap_ms"]
        if isinstance(value, bool) or not isinstance(value, (int, float)) or value < 0:
            raise RhythmError(E_RECOVERY_INVALID, "min_gap_ms must be non-negative", item.item_id)
        changes["min_gap_samples"] = round_fraction(
            Fraction(str(value)) * sample_rate / 1000
        )
    result = replace(item, **changes)
    if result.start_sample < 0 or result.end_sample <= result.start_sample:
        raise RhythmError(
            E_RECOVERY_INVALID,
            "edited interval is empty or negative",
            item.item_id,
            {"start_sample": result.start_sample, "end_sample": result.end_sample},
        )
    return result


@dataclass
class RecoveryStatus:
    recovered: bool
    base_version: int
    new_version: int
    affected_ids: list[str]
    resumed_iteration: int

    @property
    def completed_ids(self) -> list[str]:
        return list(self.affected_ids)

    def to_dict(self) -> dict[str, Any]:
        return {
            "recovered": self.recovered,
            "base_version": self.base_version,
            "new_version": self.new_version,
            "affected_ids": self.affected_ids,
            "resumed_iteration": self.resumed_iteration,
        }


class DeliverySession:
    """One material's committed version, editing loop and crash recovery."""

    def __init__(
        self,
        bundle: SourceBundle,
        frame_table: FrameTable,
        coverage_limit: int,
        store: VersionStore,
    ):
        self.bundle = bundle
        self.frame_table = frame_table
        self.coverage_limit = coverage_limit
        self.store = store
        self.snapshot = store.load()

    @classmethod
    def open(
        cls,
        bundle: SourceBundle,
        frame_table: FrameTable,
        coverage_limit: int,
        store_path: str | Path,
    ) -> "DeliverySession":
        store = VersionStore(store_path)
        session = cls(bundle, frame_table, coverage_limit, store)
        if session.snapshot is not None:
            persisted_bundle = bundle_from_canonical(session.snapshot.payload["source"])
            if persisted_bundle.material_id != bundle.material_id:
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    "snapshot belongs to another material",
                    None,
                    {
                        "snapshot_material": persisted_bundle.material_id,
                        "requested_material": bundle.material_id,
                    },
                )
            persisted_frame = parse_frame_table(persisted_bundle.frame_table_path)
            wav_info = parse_wav_info(persisted_bundle.wav_path)
            session.bundle = persisted_bundle
            session.frame_table = persisted_frame
            session.coverage_limit = min(wav_info.sample_count, persisted_frame.duration_samples)
            session.store.sync_delivery_artifacts(session.snapshot)
        return session

    def initialize(self) -> Snapshot:
        if self.snapshot is not None:
            return self.snapshot
        solver = RhythmSolver(self.bundle, self.frame_table, self.coverage_limit)
        result = solver.solve(version=1)
        txn_id = f"initial:{content_hash({'source': self.bundle.source_hash, 'version': 1})}"
        self.snapshot = self.store.save_snapshot(
            version=1,
            bundle=self.bundle,
            result=result,
            base=None,
            txn_id=txn_id,
        )
        return self.snapshot

    def update_item(
        self,
        item_id: str,
        patch: dict[str, Any],
        *,
        expected_version: int | None = None,
        cancel_check: Callable[[], bool] | None = None,
        progress: Callable[[dict[str, Any]], None] | None = None,
    ) -> Snapshot:
        if self.snapshot is None:
            raise RhythmError(E_RECOVERY_INVALID, "session has no committed baseline", item_id)
        disk = self.store.load()
        if disk is None:
            raise RhythmError(E_RECOVERY_INVALID, "committed snapshot disappeared", item_id)
        expected = expected_version if expected_version is not None else self.snapshot.version
        if disk.version != expected or disk.chain_hash != self.snapshot.chain_hash:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "in-memory version and disk state disagree",
                item_id,
                {
                    "expected_version": expected,
                    "disk_version": disk.version,
                    "expected_hash": self.snapshot.chain_hash,
                    "disk_hash": disk.chain_hash,
                },
            )
        self.snapshot = disk
        base_results = disk.result.by_id()
        if item_id not in base_results:
            raise RhythmError(E_RECOVERY_INVALID, "edited sentence does not exist", item_id)
        updated_items = []
        for item in self.bundle.items:
            updated_items.append(
                apply_item_patch(item, patch, self.bundle.sample_rate)
                if item.item_id == item_id
                else item
            )
        edited_bundle = replace(self.bundle, items=tuple(updated_items))
        for item in edited_bundle.items:
            if item.end_sample > self.coverage_limit:
                raise RhythmError(
                    E_RECOVERY_INVALID,
                    "edited sentence exceeds media coverage",
                    item.item_id,
                    {"end_sample": item.end_sample, "coverage_limit": self.coverage_limit},
                )
        solver = RhythmSolver(
            edited_bundle,
            self.frame_table,
            self.coverage_limit,
            cancel_check=cancel_check,
        )
        plan = solver.plan_impact(item_id, base_results)
        new_version = disk.version + 1
        txn_id = f"edit:{content_hash({'base': disk.chain_hash, 'item': item_id, 'patch': patch, 'target': new_version})}"
        journal: dict[str, Any] = {
            "txn_id": txn_id,
            "base_version": disk.version,
            "base_chain_hash": disk.chain_hash,
            "material_id": edited_bundle.material_id,
            "source_hash": edited_bundle.source_hash,
            "params_version": edited_bundle.params_version,
            "source": edited_bundle.canonical(),
            "changed_id": item_id,
            "affected_ids": plan.affected_ids,
            "reasons": plan.reasons,
            "checkpoint": None,
            "new_version": new_version,
        }
        self.store.write_journal(journal)

        def on_progress(checkpoint: dict[str, Any]) -> None:
            journal["checkpoint"] = checkpoint
            self.store.write_journal(journal)
            if progress is not None:
                progress(checkpoint)

        solver.progress = on_progress
        try:
            result = solver.solve(
                version=new_version,
                changed_id=item_id,
                base_results=base_results,
            )
        except RhythmError as error:
            if error.code in ("E_CANCELED", "E_DESC_CONFLICT") and isinstance(
                error.evidence.get("checkpoint"), dict
            ):
                journal["checkpoint"] = error.evidence["checkpoint"]
                self.store.write_journal(journal)
            raise
        new_snapshot = self.store.save_snapshot(
            version=new_version,
            bundle=edited_bundle,
            result=result,
            base=disk,
            txn_id=txn_id,
        )
        current_journal = self.store.load_journal()
        if current_journal and current_journal.get("txn_id") == txn_id:
            self.store.remove_journal()
        self.bundle = edited_bundle
        self.snapshot = new_snapshot
        return new_snapshot

    def recover(
        self,
        *,
        expected_version: int | None = None,
        cancel_check: Callable[[], bool] | None = None,
        progress: Callable[[dict[str, Any]], None] | None = None,
    ) -> RecoveryStatus:
        disk = self.store.load()
        if disk is None:
            raise RhythmError(E_RECOVERY_INVALID, "recovery has no committed baseline", None)
        journal = self.store.load_journal()
        if journal is None:
            self.snapshot = disk
            self.store.sync_delivery_artifacts(disk)
            return RecoveryStatus(False, disk.version, disk.version, [], 0)

        if journal["base_version"] < disk.version:
            if (
                disk.payload.get("txn_id") == journal["txn_id"]
                and int(journal["new_version"]) == disk.version
            ):
                self.store.remove_journal()
                self.snapshot = disk
                return RecoveryStatus(
                    True,
                    int(journal["base_version"]),
                    disk.version,
                    list(journal.get("affected_ids", [])),
                    0,
                )
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal points to an older snapshot and the commit is not identifiable",
                None,
                {
                    "journal_txn": journal["txn_id"],
                    "journal_base": journal["base_version"],
                    "disk_version": disk.version,
                    "disk_txn": disk.payload.get("txn_id"),
                },
            )
        if journal["base_version"] != disk.version or journal["base_chain_hash"] != disk.chain_hash:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal base does not match the committed snapshot",
                None,
                {
                    "journal_base_version": journal["base_version"],
                    "disk_version": disk.version,
                    "journal_base_hash": journal["base_chain_hash"],
                    "disk_hash": disk.chain_hash,
                },
            )
        expected = expected_version if expected_version is not None else disk.version
        if disk.version != expected:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "recovery expected version changed",
                None,
                {"expected": expected, "disk": disk.version},
            )
        edited_bundle = bundle_from_canonical(journal["source"])
        if edited_bundle.source_hash != journal["source_hash"]:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal source hash is invalid",
                None,
                {
                    "declared": journal["source_hash"],
                    "actual": edited_bundle.source_hash,
                },
            )
        existing = next(
            (item for item in edited_bundle.items if item.item_id == journal["changed_id"]),
            None,
        )
        if existing is None:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal refers to a missing edited sentence",
                journal.get("changed_id"),
            )
        self.bundle = edited_bundle
        checkpoint = journal.get("checkpoint")
        if checkpoint is not None:
            self._validate_checkpoint(checkpoint, journal)
        solver = RhythmSolver(
            edited_bundle,
            self.frame_table,
            self.coverage_limit,
            cancel_check=cancel_check,
        )

        def on_progress(next_checkpoint: dict[str, Any]) -> None:
            journal["checkpoint"] = next_checkpoint
            self.store.write_journal(journal)
            if progress is not None:
                progress(next_checkpoint)

        solver.progress = on_progress
        new_version = int(journal["new_version"])
        if new_version != disk.version + 1:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal target version is invalid",
                None,
                {"target": new_version, "base": disk.version},
            )
        try:
            result = solver.solve(
                version=new_version,
                changed_id=journal["changed_id"],
                base_results=disk.result.by_id(),
                checkpoint=checkpoint,
            )
        except RhythmError as error:
            if error.code in ("E_CANCELED", "E_DESC_CONFLICT") and isinstance(
                error.evidence.get("checkpoint"), dict
            ):
                journal["checkpoint"] = error.evidence["checkpoint"]
                self.store.write_journal(journal)
            raise
        new_snapshot = self.store.save_snapshot(
            version=new_version,
            bundle=edited_bundle,
            result=result,
            base=disk,
            txn_id=journal["txn_id"],
        )
        current_journal = self.store.load_journal()
        if current_journal and current_journal.get("txn_id") == journal["txn_id"]:
            self.store.remove_journal()
        self.snapshot = new_snapshot
        resumed_iteration = int(checkpoint.get("iteration", 0)) if checkpoint else 0
        return RecoveryStatus(
            True,
            disk.version,
            new_version,
            list(journal.get("affected_ids", [])),
            resumed_iteration,
        )

    @staticmethod
    def _validate_checkpoint(checkpoint: dict[str, Any], journal: dict[str, Any]) -> None:
        required = {
            "schema_version",
            "source_hash",
            "params_version",
            "iteration",
            "chain",
            "records",
            "works",
            "affected_ids",
            "reasons",
        }
        if not isinstance(checkpoint, dict) or required - set(checkpoint):
            missing = sorted(required - set(checkpoint)) if isinstance(checkpoint, dict) else sorted(required)
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal checkpoint is malformed",
                None,
                {"missing": missing},
            )
        if checkpoint["source_hash"] != journal["source_hash"]:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal checkpoint source does not match journal source",
                None,
            )
        if checkpoint["schema_version"] != SCHEMA_VERSION:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal checkpoint schema is unsupported",
                None,
                {"schema_version": checkpoint["schema_version"]},
            )
        if checkpoint["params_version"] != journal["params_version"]:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal checkpoint parameters do not match journal",
                None,
            )
        iteration = checkpoint["iteration"]
        max_iterations = int(journal["source"].get("max_iterations", 256))
        if (
            isinstance(iteration, bool)
            or not isinstance(iteration, int)
            or iteration < 0
            or iteration > max_iterations
        ):
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal checkpoint iteration is invalid",
                None,
                {"iteration": iteration, "max_iterations": max_iterations},
            )
        if not isinstance(checkpoint["chain"], list) or not isinstance(checkpoint["records"], list):
            raise RhythmError(E_RECOVERY_INVALID, "journal checkpoint trace is malformed", None)
        affected = set(checkpoint["affected_ids"])
        works = set(checkpoint["works"])
        source_ids = {item["id"] for item in journal["source"]["items"]}
        if not affected <= works or works != source_ids:
            raise RhythmError(
                E_RECOVERY_INVALID,
                "journal checkpoint sentence state is incomplete",
                None,
                {
                    "missing_affected": sorted(affected - works),
                    "missing_source": sorted(source_ids - works),
                    "unknown_works": sorted(works - source_ids),
                },
            )


def reconstruct_source_for_recovery(
    bundle: SourceBundle,
    frame_table_path: str | None = None,
) -> tuple[SourceBundle, FrameTable, int]:
    wav_info = parse_wav_info(bundle.wav_path)
    frame_table = parse_frame_table(frame_table_path or bundle.frame_table_path)
    return bundle, frame_table, min(wav_info.sample_count, frame_table.duration_samples)
