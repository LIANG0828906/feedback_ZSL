from __future__ import annotations

from typing import Any, Mapping

from .canonical import normalize_value, sha256_obj
from .models import PublicationChainEntry, VersionRecord, freeze_value
from .color import max_channel_delta


def version_content_payload(
    *,
    material_id: str,
    version: int,
    parent_version: int | None,
    params: Mapping[str, Any],
    dependencies: Mapping[str, Any],
    colors: Mapping[str, Any],
    intermediates: Mapping[str, Any],
) -> dict[str, Any]:
    return {
        "material_id": material_id,
        "version": version,
        "parent_version": parent_version,
        "params": params,
        "dependencies": dependencies,
        "colors": colors,
        "intermediates": intermediates,
    }


def version_content_hash(
    *,
    material_id: str,
    version: int,
    parent_version: int | None,
    params: Mapping[str, Any],
    dependencies: Mapping[str, Any],
    colors: Mapping[str, Any],
    intermediates: Mapping[str, Any],
) -> str:
    return sha256_obj(
        version_content_payload(
            material_id=material_id,
            version=version,
            parent_version=parent_version,
            params=params,
            dependencies=dependencies,
            colors=colors,
            intermediates=intermediates,
        )
    )


def version_chain_hash(
    *,
    previous_chain_hash: str,
    material_id: str,
    version: int,
    content_hash: str,
) -> str:
    return sha256_obj(
        {
            "previous_chain_hash": previous_chain_hash,
            "material_id": material_id,
            "version": version,
            "content_hash": content_hash,
        }
    )


def make_version_record(
    *,
    material_id: str,
    version: int,
    parent_version: int | None,
    params: Mapping[str, Any],
    dependencies: Mapping[str, Any],
    colors: Mapping[str, Any],
    intermediates: Mapping[str, Any],
    previous_chain_hash: str,
) -> VersionRecord:
    normalized_params = normalize_value(params)
    normalized_dependencies = normalize_value(dependencies)
    normalized_colors = normalize_value(colors)
    normalized_intermediates = normalize_value(intermediates)
    content_hash = version_content_hash(
        material_id=material_id,
        version=version,
        parent_version=parent_version,
        params=normalized_params,
        dependencies=normalized_dependencies,
        colors=normalized_colors,
        intermediates=normalized_intermediates,
    )
    chain_hash = version_chain_hash(
        previous_chain_hash=previous_chain_hash,
        material_id=material_id,
        version=version,
        content_hash=content_hash,
    )
    return VersionRecord(
        material_id=material_id,
        version=version,
        parent_version=parent_version,
        params=freeze_value(normalized_params),
        dependencies=freeze_value(normalized_dependencies),
        colors=freeze_value(normalized_colors),
        intermediates=freeze_value(normalized_intermediates),
        content_hash=content_hash,
        chain_hash=chain_hash,
    )


def verify_version_record(
    record: VersionRecord, *, previous_chain_hash: str
) -> None:
    expected_content = version_content_hash(
        material_id=record.material_id,
        version=record.version,
        parent_version=record.parent_version,
        params=record.params,
        dependencies=record.dependencies,
        colors=record.colors,
        intermediates=record.intermediates,
    )
    if expected_content != record.content_hash:
        raise ValueError(
            f"content hash mismatch for {record.material_id} v{record.version}"
        )
    expected_chain = version_chain_hash(
        previous_chain_hash=previous_chain_hash,
        material_id=record.material_id,
        version=record.version,
        content_hash=record.content_hash,
    )
    if expected_chain != record.chain_hash:
        raise ValueError(
            f"chain hash mismatch for {record.material_id} v{record.version}"
        )


def publication_hash(
    *,
    generation: int,
    previous_publication_hash: str,
    pipeline_hash: str,
    material_versions: Mapping[str, int],
    record_hashes: Mapping[str, str],
    chain_hashes: Mapping[str, str],
) -> str:
    return sha256_obj(
        {
            "generation": generation,
            "previous_publication_hash": previous_publication_hash,
            "pipeline_hash": pipeline_hash,
            "material_versions": material_versions,
            "record_hashes": record_hashes,
            "chain_hashes": chain_hashes,
        }
    )


def make_publication_entry(
    *,
    generation: int,
    previous_publication_hash: str,
    pipeline_hash: str,
    records: Mapping[str, VersionRecord],
) -> PublicationChainEntry:
    material_versions = {
        material_id: record.version
        for material_id, record in sorted(records.items())
    }
    record_hashes = {
        material_id: record.content_hash
        for material_id, record in sorted(records.items())
    }
    chain_hashes = {
        material_id: record.chain_hash
        for material_id, record in sorted(records.items())
    }
    value = publication_hash(
        generation=generation,
        previous_publication_hash=previous_publication_hash,
        pipeline_hash=pipeline_hash,
        material_versions=material_versions,
        record_hashes=record_hashes,
        chain_hashes=chain_hashes,
    )
    return PublicationChainEntry(
        generation=generation,
        previous_publication_hash=previous_publication_hash,
        publication_hash=value,
        pipeline_hash=pipeline_hash,
        material_versions=freeze_value(material_versions),
        record_hashes=freeze_value(record_hashes),
        chain_hashes=freeze_value(chain_hashes),
    )


def verify_publication_entry(entry: PublicationChainEntry) -> None:
    expected = publication_hash(
        generation=entry.generation,
        previous_publication_hash=entry.previous_publication_hash,
        pipeline_hash=entry.pipeline_hash,
        material_versions=entry.material_versions,
        record_hashes=entry.record_hashes,
        chain_hashes=entry.chain_hashes,
    )
    if expected != entry.publication_hash:
        raise ValueError(
            f"publication hash mismatch for generation {entry.generation}"
        )


def diff_versions(old: VersionRecord, new: VersionRecord) -> dict[str, Any]:
    changed_dependencies = sorted(
        key
        for key in set(old.dependencies) | set(new.dependencies)
        if old.dependencies.get(key) != new.dependencies.get(key)
    )
    return {
        "material_id": new.material_id,
        "from_version": old.version,
        "to_version": new.version,
        "changed_dependency_factors": changed_dependencies,
        "params_changed": old.params != new.params,
        "intermediates_changed": old.intermediates != new.intermediates,
        "colors_changed": old.colors != new.colors,
        "max_channel_deltas": max_channel_delta(old.colors, new.colors),
        "old_colors": dict(old.colors),
        "new_colors": dict(new.colors),
        "old_content_hash": old.content_hash,
        "new_content_hash": new.content_hash,
    }
