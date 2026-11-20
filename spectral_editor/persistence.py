from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Mapping

from .canonical import (
    atomic_write_bytes,
    canonical_bytes,
    read_json,
    sha256_bytes,
)
from .errors import BusinessError, recovery_invalid
from .models import (
    MaterialHistory,
    PendingJob,
    PipelineConfig,
    PublicationChainEntry,
    PublishedSnapshot,
    VersionRecord,
    freeze_value,
    thaw_value,
)
from .versioning import verify_publication_entry, verify_version_record


SNAPSHOT_SCHEMA = "spectral-editor-snapshot/v1"
PENDING_SCHEMA = "spectral-editor-pending/v1"
RECOVERY_ANCHOR_SCHEMA = "spectral-editor-recovery-anchor/v1"


def snapshot_document(snapshot: PublishedSnapshot) -> dict[str, Any]:
    materials: dict[str, Any] = {}
    for material_id in sorted(snapshot.histories):
        history = snapshot.histories[material_id]
        materials[material_id] = {
            "current_version": history.current().version,
            "versions": [record.to_dict() for record in history.versions],
        }
    return {
        "schema": SNAPSHOT_SCHEMA,
        "library_id": snapshot.library_id,
        "seed": snapshot.seed,
        "generation": snapshot.generation,
        "previous_generation": snapshot.previous_generation,
        "publication_hash": snapshot.publication_hash,
        "previous_publication_hash": snapshot.previous_publication_hash,
        "pipeline": snapshot.pipeline.to_dict(),
        "materials": materials,
        "publication_chain": [entry.to_dict() for entry in snapshot.publication_chain],
        "ui_state": thaw_value(snapshot.ui_state),
    }


def snapshot_bytes(snapshot: PublishedSnapshot) -> bytes:
    return canonical_bytes(snapshot_document(snapshot), pretty=True)


def save_snapshot(path: str | Path, snapshot: PublishedSnapshot) -> str:
    data = snapshot_bytes(snapshot)
    atomic_write_bytes(path, data)
    return sha256_bytes(data)


def recovery_anchor_path_for_snapshot(path: str | Path) -> Path:
    return Path(path).parent / "recovery_anchor.json"


def save_recovery_anchor(snapshot_path: str | Path, snapshot: PublishedSnapshot) -> str:
    document = {
        "schema": RECOVERY_ANCHOR_SCHEMA,
        "library_id": snapshot.library_id,
        "generation": snapshot.generation,
        "publication_hash": snapshot.publication_hash,
    }
    return atomic_write_bytes(
        recovery_anchor_path_for_snapshot(snapshot_path),
        canonical_bytes(document, pretty=True),
    )


def verify_recovery_anchor(
    snapshot_path: str | Path, snapshot: PublishedSnapshot
) -> None:
    anchor_path = recovery_anchor_path_for_snapshot(snapshot_path)
    if not anchor_path.exists():
        return
    try:
        document = read_json(anchor_path)
    except (OSError, json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise recovery_invalid(
            f"recovery anchor is invalid: {exc}", "anchor"
        ) from exc
    if not isinstance(document, Mapping):
        raise recovery_invalid("recovery anchor must be an object", "anchor")
    if document.get("schema") != RECOVERY_ANCHOR_SCHEMA:
        raise recovery_invalid("unsupported recovery anchor schema", "anchor.schema")
    generation = document.get("generation")
    publication = document.get("publication_hash")
    library_id = document.get("library_id")
    if isinstance(generation, bool) or not isinstance(generation, int):
        raise recovery_invalid("anchor generation must be an integer", "anchor.generation")
    if not isinstance(publication, str) or not publication:
        raise recovery_invalid(
            "anchor publication_hash must be non-empty", "anchor.publication_hash"
        )
    if library_id != snapshot.library_id:
        raise recovery_invalid(
            "recovery anchor belongs to a different library", "anchor.library_id"
        )
    if snapshot.generation < generation:
        raise recovery_invalid(
            "snapshot generation is older than the committed recovery anchor",
            "snapshot.generation",
        )
    if snapshot.generation == generation and snapshot.publication_hash != publication:
        raise recovery_invalid(
            "snapshot publication hash differs from the recovery anchor",
            "snapshot.publication_hash",
        )


def _required(mapping: Mapping[str, Any], key: str, path: str) -> Any:
    if key not in mapping:
        raise recovery_invalid("required snapshot field is missing", f"{path}.{key}")
    return mapping[key]


def _parse_version_record(
    value: Any, path: str, expected_material_id: str, previous_chain_hash: str
) -> VersionRecord:
    if not isinstance(value, Mapping):
        raise recovery_invalid("version record must be an object", path)
    required = (
        "material_id",
        "version",
        "parent_version",
        "params",
        "dependencies",
        "colors",
        "intermediates",
        "content_hash",
        "chain_hash",
    )
    for key in required:
        _required(value, key, path)
    try:
        record = VersionRecord.from_dict(value)
    except (TypeError, ValueError, KeyError) as exc:
        raise recovery_invalid(f"invalid version record: {exc}", path) from exc
    if record.material_id != expected_material_id:
        raise recovery_invalid(
            "version record material_id does not match its material key",
            f"{path}.material_id",
        )
    if record.version < 1:
        raise recovery_invalid("version number must be positive", f"{path}.version")
    try:
        verify_version_record(record, previous_chain_hash=previous_chain_hash)
    except ValueError as exc:
        raise recovery_invalid(str(exc), path) from exc
    return record


def _parse_histories(document: Mapping[str, Any]) -> dict[str, MaterialHistory]:
    materials = _required(document, "materials", "snapshot")
    if not isinstance(materials, Mapping) or not materials:
        raise recovery_invalid("materials must be a non-empty object", "snapshot.materials")
    histories: dict[str, MaterialHistory] = {}
    for material_id in sorted(materials):
        raw_history = materials[material_id]
        path = f"snapshot.materials.{material_id}"
        if not isinstance(raw_history, Mapping):
            raise recovery_invalid("material history must be an object", path)
        raw_versions = _required(raw_history, "versions", path)
        if not isinstance(raw_versions, list) or not raw_versions:
            raise recovery_invalid("versions must be a non-empty array", f"{path}.versions")
        current_version = _required(raw_history, "current_version", path)
        records: list[VersionRecord] = []
        previous_chain = ""
        for index, raw_record in enumerate(raw_versions):
            record_path = f"{path}.versions[{index}]"
            record = _parse_version_record(
                raw_record, record_path, material_id, previous_chain
            )
            expected_version = index + 1
            if record.version != expected_version:
                raise recovery_invalid(
                    "material version sequence is not contiguous starting at 1",
                    f"{record_path}.version",
                )
            if index == 0:
                if record.parent_version is not None:
                    raise recovery_invalid(
                        "first version cannot have a parent",
                        f"{record_path}.parent_version",
                    )
            elif record.parent_version != records[-1].version:
                raise recovery_invalid(
                    "parent_version does not match the preceding version",
                    f"{record_path}.parent_version",
                )
            records.append(record)
            previous_chain = record.chain_hash
        if current_version != records[-1].version:
            raise recovery_invalid(
                "current_version does not match the last stored version",
                f"{path}.current_version",
            )
        histories[material_id] = MaterialHistory(material_id, tuple(records))
    return histories


def _parse_chain(
    document: Mapping[str, Any],
    histories: Mapping[str, MaterialHistory],
    pipeline: PipelineConfig,
) -> tuple[PublicationChainEntry, ...]:
    raw_chain = _required(document, "publication_chain", "snapshot")
    if not isinstance(raw_chain, list) or not raw_chain:
        raise recovery_invalid(
            "publication_chain must be a non-empty array",
            "snapshot.publication_chain",
        )
    entries: list[PublicationChainEntry] = []
    previous_hash = ""
    previous_versions: dict[str, int] = {}
    material_ids = set(histories)
    for index, raw_entry in enumerate(raw_chain):
        path = f"snapshot.publication_chain[{index}]"
        if not isinstance(raw_entry, Mapping):
            raise recovery_invalid("publication entry must be an object", path)
        try:
            entry = PublicationChainEntry.from_dict(raw_entry)
        except (TypeError, ValueError, KeyError) as exc:
            raise recovery_invalid(f"invalid publication entry: {exc}", path) from exc
        if entry.generation != index + 1:
            raise recovery_invalid(
                "publication generations must be contiguous starting at 1",
                f"{path}.generation",
            )
        if entry.previous_publication_hash != previous_hash:
            raise recovery_invalid(
                "publication hash chain is broken",
                f"{path}.previous_publication_hash",
            )
        try:
            verify_publication_entry(entry)
        except ValueError as exc:
            raise recovery_invalid(str(exc), path) from exc
        if set(entry.material_versions) != material_ids:
            raise recovery_invalid(
                "publication material set does not match snapshot materials",
                f"{path}.material_versions",
            )
        if index == len(raw_chain) - 1 and entry.pipeline_hash != pipeline.config_hash():
            raise recovery_invalid(
                "publication pipeline hash does not match snapshot pipeline",
                f"{path}.pipeline_hash",
            )
        for material_id, version in entry.material_versions.items():
            previous_version = previous_versions.get(material_id, 0)
            if version < previous_version:
                raise recovery_invalid(
                    "material version rolled back across publications",
                    f"{path}.material_versions.{material_id}",
                )
            if version > previous_version + 1:
                raise recovery_invalid(
                    "material version skipped across publications",
                    f"{path}.material_versions.{material_id}",
                )
            if version == previous_version:
                previous_entry = entries[-1] if entries else None
                if previous_entry is not None:
                    if (
                        previous_entry.record_hashes[material_id]
                        != entry.record_hashes[material_id]
                        or previous_entry.chain_hashes[material_id]
                        != entry.chain_hashes[material_id]
                    ):
                        raise recovery_invalid(
                            "reused published version was rewritten",
                            f"{path}.record_hashes.{material_id}",
                        )
            try:
                record = histories[material_id].get(version)
            except KeyError as exc:
                raise recovery_invalid(
                    "publication references a missing version",
                    f"{path}.material_versions.{material_id}",
                ) from exc
            if entry.record_hashes[material_id] != record.content_hash:
                raise recovery_invalid(
                    "publication content hash does not match stored version",
                    f"{path}.record_hashes.{material_id}",
                )
            if entry.chain_hashes[material_id] != record.chain_hash:
                raise recovery_invalid(
                    "publication chain hash does not match stored version",
                    f"{path}.chain_hashes.{material_id}",
                )
        entries.append(entry)
        previous_hash = entry.publication_hash
        previous_versions = dict(entry.material_versions)
    last = entries[-1]
    for material_id, history in histories.items():
        current = history.current()
        if last.material_versions[material_id] != current.version:
            raise recovery_invalid(
                "latest publication does not reference the current material version",
                f"snapshot.materials.{material_id}.current_version",
            )
    return tuple(entries)


def parse_snapshot_document(document: Any) -> PublishedSnapshot:
    if not isinstance(document, Mapping):
        raise recovery_invalid("snapshot root must be an object", "snapshot")
    if document.get("schema") != SNAPSHOT_SCHEMA:
        raise recovery_invalid(
            f"unsupported snapshot schema {document.get('schema')!r}", "snapshot.schema"
        )
    for key in (
        "library_id",
        "seed",
        "generation",
        "previous_generation",
        "publication_hash",
        "previous_publication_hash",
        "pipeline",
        "ui_state",
    ):
        _required(document, key, "snapshot")
    try:
        pipeline = PipelineConfig.from_dict(document["pipeline"], "snapshot.pipeline")
    except BusinessError as exc:
        raise recovery_invalid(
            f"invalid pipeline in snapshot: {exc.message}", exc.path or "snapshot.pipeline"
        ) from exc
    try:
        histories = _parse_histories(document)
        chain = _parse_chain(document, histories, pipeline)
    except BusinessError:
        raise
    except (TypeError, ValueError, KeyError) as exc:
        raise recovery_invalid(f"invalid snapshot structure: {exc}") from exc
    generation = int(document["generation"])
    previous_generation = int(document["previous_generation"])
    if generation != len(chain):
        raise recovery_invalid(
            "generation does not match publication chain length",
            "snapshot.generation",
        )
    expected_previous_generation = 0 if generation == 1 else generation - 1
    if previous_generation != expected_previous_generation:
        raise recovery_invalid(
            "previous_generation is inconsistent", "snapshot.previous_generation"
        )
    expected_previous_hash = chain[-2].publication_hash if generation > 1 else ""
    if document["previous_publication_hash"] != expected_previous_hash:
        raise recovery_invalid(
            "previous_publication_hash is inconsistent",
            "snapshot.previous_publication_hash",
        )
    if document["publication_hash"] != chain[-1].publication_hash:
        raise recovery_invalid(
            "top-level publication hash does not match the chain",
            "snapshot.publication_hash",
        )
    ui_state = document["ui_state"]
    if not isinstance(ui_state, Mapping):
        raise recovery_invalid("ui_state must be an object", "snapshot.ui_state")
    for key in ("selected_material", "selected_version", "timeline_material"):
        if key not in ui_state:
            raise recovery_invalid(
                "required UI state field is missing", f"snapshot.ui_state.{key}"
            )
    selected_material = ui_state["selected_material"]
    timeline_material = ui_state["timeline_material"]
    selected_version = ui_state["selected_version"]
    if selected_material not in histories:
        raise recovery_invalid(
            "selected material does not exist",
            "snapshot.ui_state.selected_material",
        )
    if timeline_material not in histories:
        raise recovery_invalid(
            "timeline material does not exist",
            "snapshot.ui_state.timeline_material",
        )
    if isinstance(selected_version, bool) or not isinstance(selected_version, int):
        raise recovery_invalid(
            "selected_version must be an integer",
            "snapshot.ui_state.selected_version",
        )
    try:
        histories[selected_material].get(selected_version)
    except KeyError as exc:
        raise recovery_invalid(
            "selected version does not exist",
            "snapshot.ui_state.selected_version",
        ) from exc
    if not isinstance(document["library_id"], str) or not document["library_id"]:
        raise recovery_invalid("library_id must be non-empty", "snapshot.library_id")
    seed = document["seed"]
    if isinstance(seed, bool) or not isinstance(seed, int):
        raise recovery_invalid("seed must be an integer", "snapshot.seed")
    return PublishedSnapshot(
        library_id=document["library_id"],
        seed=seed,
        generation=generation,
        previous_generation=previous_generation,
        publication_hash=document["publication_hash"],
        previous_publication_hash=document["previous_publication_hash"],
        pipeline=pipeline,
        histories=freeze_value(histories),
        publication_chain=chain,
        ui_state=freeze_value(ui_state),
    )


def load_snapshot(path: str | Path) -> PublishedSnapshot:
    source = Path(path)
    try:
        raw = source.read_bytes()
    except FileNotFoundError as exc:
        raise recovery_invalid(f"snapshot not found: {source}", "snapshot") from exc
    except OSError as exc:
        raise recovery_invalid(f"cannot read snapshot: {exc}", "snapshot") from exc
    try:
        document = json.loads(raw.decode("utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise recovery_invalid(f"snapshot JSON is invalid: {exc}", "snapshot") from exc
    snapshot = parse_snapshot_document(document)
    verify_recovery_anchor(source, snapshot)
    return snapshot


def save_pending(path: str | Path, pending: PendingJob) -> str:
    data = canonical_bytes(pending.to_dict(), pretty=True)
    atomic_write_bytes(path, data)
    return sha256_bytes(data)


def load_pending(path: str | Path) -> PendingJob | None:
    source = Path(path)
    if not source.exists():
        return None
    try:
        document = read_json(source)
    except (OSError, json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise recovery_invalid(f"pending journal is invalid: {exc}", "pending") from exc
    if not isinstance(document, Mapping):
        raise recovery_invalid("pending journal root must be an object", "pending")
    if document.get("schema") != PENDING_SCHEMA:
        raise recovery_invalid("unsupported pending journal schema", "pending.schema")
    try:
        return PendingJob.from_dict(document)
    except BusinessError as exc:
        raise recovery_invalid(
            f"invalid pending journal: {exc.message}", exc.path or "pending"
        ) from exc
    except (TypeError, ValueError, KeyError) as exc:
        raise recovery_invalid(f"invalid pending journal: {exc}", "pending") from exc


def reconcile_pending(
    snapshot: PublishedSnapshot, pending_path: str | Path
) -> PendingJob | None:
    pending = load_pending(pending_path)
    if pending is None:
        return None
    if pending.library_id != snapshot.library_id:
        raise recovery_invalid(
            "pending journal belongs to a different library", "pending.library_id"
        )
    if pending.base_generation == snapshot.generation:
        if pending.base_publication_hash != snapshot.publication_hash:
            raise recovery_invalid(
                "pending journal base publication does not match snapshot",
                "pending.base_publication_hash",
            )
        return pending
    if pending.base_generation + 1 == snapshot.generation:
        last = snapshot.publication_chain[-1]
        if (
            last.previous_publication_hash == pending.base_publication_hash
            and snapshot.pipeline.to_dict() == pending.requested_pipeline.to_dict()
        ):
            Path(pending_path).unlink(missing_ok=True)
            return None
    raise recovery_invalid(
        "pending journal generation does not match the published snapshot",
        "pending.base_generation",
    )
