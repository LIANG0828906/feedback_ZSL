from __future__ import annotations

import contextlib
import fcntl
import json
import os
import tempfile
from pathlib import Path
from typing import Any, Iterator

from .canonical import canonical_json_bytes, normalize, sha256_bytes, sha256_json
from .errors import (
    E_BUSY,
    E_COMMIT_FAILED,
    E_RECOVERY_INVALID,
    PipelineError,
    fail,
)
from .scene import Scene
from .strokes import BatchData

BATCH_MAGIC = b"NPRB1\n"
MANIFEST_FORMAT = "npr-manifest-v1"
CURRENT_FORMAT = "npr-current-v1"
STAGING_FORMAT = "npr-staging-v1"
SCHEMA_VERSION = 1


def _fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(
        prefix=f".{path.name}.tmp.",
        dir=str(path.parent),
    )
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_directory(path.parent)
    except BaseException:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temporary)
        raise


def _safe_relative_path(root: Path, relative: str, location: str) -> Path:
    candidate = (root / relative).resolve()
    resolved_root = root.resolve()
    try:
        candidate.relative_to(resolved_root)
    except ValueError:
        fail(E_RECOVERY_INVALID, location, "path_escape", path=relative)
    return candidate


def encode_batch(
    batch: BatchData,
    factor_versions: dict[str, str],
) -> tuple[bytes, dict[str, Any], str]:
    document = {
        "format": "npr-batch-v1",
        "schema_version": SCHEMA_VERSION,
        "layer": batch.layer,
        "batch": batch.batch_id,
        "chunk": batch.chunk_id,
        "zone_id": batch.zone_id,
        "factor_versions": dict(sorted(factor_versions.items())),
        "bbox": list(batch.bbox),
        "strokes": [
            stroke.to_dict(index) for index, stroke in enumerate(batch.strokes)
        ],
    }
    payload = canonical_json_bytes(document)
    data = BATCH_MAGIC + payload + b"\n"
    return data, document, sha256_bytes(data)


def decode_batch(data: bytes, location: str = "batch") -> tuple[dict[str, Any], str]:
    if not data.startswith(BATCH_MAGIC):
        fail(E_RECOVERY_INVALID, location, "invalid_batch_magic")
    digest = sha256_bytes(data)
    payload = data[len(BATCH_MAGIC) :]
    if payload.endswith(b"\n"):
        payload = payload[:-1]
    try:
        document = json.loads(payload.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        fail(E_RECOVERY_INVALID, location, "invalid_batch_json")
    if document.get("format") != "npr-batch-v1":
        fail(E_RECOVERY_INVALID, location, "invalid_batch_format")
    if canonical_json_bytes(document) != payload:
        fail(E_RECOVERY_INVALID, location, "non_canonical_batch")
    return document, digest


def manifest_content_payload(manifest: dict[str, Any]) -> dict[str, Any]:
    batches = []
    for record in sorted(
        manifest.get("batches", []),
        key=lambda item: (int(item.get("layer", -1)), str(item.get("batch", ""))),
    ):
        batches.append(
            {
                "layer": record.get("layer"),
                "batch": record.get("batch"),
                "chunk": record.get("chunk"),
                "zone_id": record.get("zone_id"),
                "hash": record.get("hash"),
                "factor_versions": record.get("factor_versions"),
                "stroke_count": record.get("stroke_count"),
                "bbox": record.get("bbox"),
            }
        )
    return {
        "format": "npr-render-content-v1",
        "scene_id": manifest.get("scene_id"),
        "factors": manifest.get("factors"),
        "layer_stats": manifest.get("layer_stats"),
        "batches": batches,
    }


def compute_manifest_hashes(manifest: dict[str, Any]) -> tuple[str, str]:
    content_hash = sha256_json(manifest_content_payload(manifest))
    unsigned = {key: value for key, value in manifest.items() if key != "manifest_hash"}
    manifest_hash = sha256_json(unsigned)
    return content_hash, manifest_hash


class Store:
    def __init__(self, root: str | Path):
        self.root = Path(root)
        self.manifests_dir = self.root / "manifests"
        self.chunks_dir = self.root / "chunks"
        self.staging_dir = self.root / "staging"
        self.current_path = self.root / "CURRENT.json"
        self.lock_path = self.root / "work.lock"

    def initialize(self) -> None:
        self.manifests_dir.mkdir(parents=True, exist_ok=True)
        self.chunks_dir.mkdir(parents=True, exist_ok=True)
        self.staging_dir.mkdir(parents=True, exist_ok=True)

    @contextlib.contextmanager
    def acquire_work_lock(self) -> Iterator[None]:
        self.initialize()
        handle = open(self.lock_path, "a+b")
        try:
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                fail(E_BUSY, "work.lock", "render_already_running")
            yield
        finally:
            with contextlib.suppress(OSError):
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
            handle.close()

    def manifest_relative(self, version: int) -> str:
        return f"manifests/v{version:06d}.json"

    def manifest_path(self, version: int) -> Path:
        return self.root / self.manifest_relative(version)

    def read_json_file(self, path: Path, location: str, code: str) -> Any:
        try:
            raw = json.loads(path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            fail(code, location, "missing_file")
        except OSError as exc:
            fail(code, location, "read_failed", reason=str(exc))
        except json.JSONDecodeError:
            fail(code, location, "invalid_json")
        return raw

    def read_current_pointer(self) -> dict[str, Any] | None:
        if not self.current_path.exists():
            manifests = list(self.manifests_dir.glob("v*.json")) if self.manifests_dir.exists() else []
            if manifests:
                fail(E_RECOVERY_INVALID, "CURRENT", "missing_current_pointer")
            return None
        pointer = self.read_json_file(self.current_path, "CURRENT", E_RECOVERY_INVALID)
        if not isinstance(pointer, dict):
            fail(E_RECOVERY_INVALID, "CURRENT", "invalid_pointer")
        if pointer.get("format") != CURRENT_FORMAT:
            fail(E_RECOVERY_INVALID, "CURRENT", "invalid_pointer_format")
        version = pointer.get("version")
        if isinstance(version, bool) or not isinstance(version, int) or version < 1:
            fail(E_RECOVERY_INVALID, "CURRENT", "invalid_manifest_version")
        manifest_name = pointer.get("manifest")
        if not isinstance(manifest_name, str):
            fail(E_RECOVERY_INVALID, "CURRENT", "invalid_manifest_path")
        expected_name = self.manifest_relative(version)
        if manifest_name != expected_name:
            fail(E_RECOVERY_INVALID, "CURRENT", "manifest_path_version_mismatch")
        if not isinstance(pointer.get("manifest_hash"), str):
            fail(E_RECOVERY_INVALID, "CURRENT", "missing_manifest_hash")
        return pointer

    def read_manifest_file(self, version: int) -> dict[str, Any]:
        location = self.manifest_relative(version)
        manifest = self.read_json_file(
            self.manifest_path(version),
            location,
            E_RECOVERY_INVALID,
        )
        self._validate_manifest_structure(manifest, version, location)
        return manifest

    def _validate_manifest_structure(
        self,
        manifest: Any,
        expected_version: int,
        location: str,
    ) -> None:
        if not isinstance(manifest, dict):
            fail(E_RECOVERY_INVALID, location, "manifest_not_object")
        if manifest.get("format") != MANIFEST_FORMAT:
            fail(E_RECOVERY_INVALID, location, "invalid_manifest_format")
        if manifest.get("schema_version") != SCHEMA_VERSION:
            fail(E_RECOVERY_INVALID, location, "manifest_schema_version_mismatch")
        version = manifest.get("version")
        if isinstance(version, bool) or not isinstance(version, int):
            fail(E_RECOVERY_INVALID, location, "invalid_manifest_version")
        if version != expected_version:
            fail(E_RECOVERY_INVALID, location, "manifest_version_mismatch")
        if not isinstance(manifest.get("scene_id"), str):
            fail(E_RECOVERY_INVALID, location, "missing_scene_id")
        if not isinstance(manifest.get("factors"), dict):
            fail(E_RECOVERY_INVALID, location, "missing_factors")
        if not isinstance(manifest.get("batches"), list):
            fail(E_RECOVERY_INVALID, location, "missing_batches")
        if not isinstance(manifest.get("layer_stats"), dict):
            fail(E_RECOVERY_INVALID, location, "missing_layer_stats")
        if not isinstance(manifest.get("content_hash"), str):
            fail(E_RECOVERY_INVALID, location, "missing_content_hash")
        if not isinstance(manifest.get("manifest_hash"), str):
            fail(E_RECOVERY_INVALID, location, "missing_manifest_hash")
        if version == 1:
            if manifest.get("previous_version") is not None:
                fail(E_RECOVERY_INVALID, location, "invalid_root_previous_version")
            if manifest.get("previous_manifest_hash") is not None:
                fail(E_RECOVERY_INVALID, location, "invalid_root_previous_hash")
        else:
            if manifest.get("previous_version") != version - 1:
                fail(E_RECOVERY_INVALID, location, "previous_version_mismatch")
            if not isinstance(manifest.get("previous_manifest_hash"), str):
                fail(E_RECOVERY_INVALID, location, "missing_previous_manifest_hash")
        seen: set[str] = set()
        for index, record in enumerate(manifest["batches"]):
            record_location = f"{location}.batches[{index}]"
            if not isinstance(record, dict):
                fail(E_RECOVERY_INVALID, record_location, "batch_record_not_object")
            required = (
                "layer",
                "batch",
                "chunk",
                "zone_id",
                "file",
                "hash",
                "factor_versions",
                "stroke_count",
            )
            for key in required:
                if key not in record:
                    fail(E_RECOVERY_INVALID, record_location, "missing_batch_field", field=key)
            if not isinstance(record["layer"], int) or record["layer"] not in (0, 1, 2):
                fail(E_RECOVERY_INVALID, record_location, "invalid_layer")
            if not isinstance(record["batch"], str) or record["batch"] in seen:
                fail(E_RECOVERY_INVALID, record_location, "duplicate_or_invalid_batch")
            seen.add(record["batch"])
            if not isinstance(record["file"], str):
                fail(E_RECOVERY_INVALID, record_location, "invalid_batch_file")
            if not isinstance(record["hash"], str):
                fail(E_RECOVERY_INVALID, record_location, "invalid_batch_hash")
            if not isinstance(record["factor_versions"], dict):
                fail(E_RECOVERY_INVALID, record_location, "invalid_factor_versions")

    def verify_manifest(self, manifest: dict[str, Any], location: str) -> None:
        expected_content, expected_manifest_hash = compute_manifest_hashes(manifest)
        if manifest.get("content_hash") != expected_content:
            fail(E_RECOVERY_INVALID, location, "content_hash_mismatch")
        if manifest.get("manifest_hash") != expected_manifest_hash:
            fail(E_RECOVERY_INVALID, location, "manifest_hash_mismatch")
        for record in manifest["batches"]:
            batch_location = f"batch:{record['batch']}"
            path = _safe_relative_path(self.root, record["file"], batch_location)
            try:
                data = path.read_bytes()
            except FileNotFoundError:
                fail(E_RECOVERY_INVALID, batch_location, "missing_chunk", file=record["file"])
            except OSError as exc:
                fail(E_RECOVERY_INVALID, batch_location, "chunk_read_failed", reason=str(exc))
            digest = sha256_bytes(data)
            if digest != record["hash"]:
                fail(E_RECOVERY_INVALID, batch_location, "chunk_hash_mismatch")
            document, decoded_hash = decode_batch(data, batch_location)
            if decoded_hash != digest:
                fail(E_RECOVERY_INVALID, batch_location, "chunk_decode_hash_mismatch")
            if document.get("batch") != record["batch"]:
                fail(E_RECOVERY_INVALID, batch_location, "batch_id_mismatch")
            if document.get("layer") != record["layer"]:
                fail(E_RECOVERY_INVALID, batch_location, "layer_mismatch")
            if document.get("chunk") != record["chunk"]:
                fail(E_RECOVERY_INVALID, batch_location, "chunk_id_mismatch")
            if document.get("factor_versions") != record["factor_versions"]:
                fail(E_RECOVERY_INVALID, batch_location, "factor_versions_mismatch")
            if len(document.get("strokes", [])) != record["stroke_count"]:
                fail(E_RECOVERY_INVALID, batch_location, "stroke_count_mismatch")

    def verify_chain(self) -> dict[str, Any] | None:
        pointer = self.read_current_pointer()
        if pointer is None:
            return None
        version = pointer["version"]
        current: dict[str, Any] | None = None
        expected_version = version
        expected_previous = pointer["manifest_hash"]
        while expected_version >= 1:
            location = self.manifest_relative(expected_version)
            manifest = self.read_manifest_file(expected_version)
            self.verify_manifest(manifest, location)
            if expected_version == version:
                if manifest["manifest_hash"] != pointer["manifest_hash"]:
                    fail(E_RECOVERY_INVALID, "CURRENT", "current_manifest_hash_mismatch")
                current = manifest
            if manifest["manifest_hash"] != expected_previous:
                fail(
                    E_RECOVERY_INVALID,
                    f"{location}:manifest_hash",
                    "hash_chain_break",
                )
            expected_previous = manifest.get("previous_manifest_hash")
            if expected_version == 1:
                if expected_previous is not None:
                    fail(
                        E_RECOVERY_INVALID,
                        f"{location}:previous_manifest_hash",
                        "hash_chain_break",
                    )
                break
            if not isinstance(expected_previous, str):
                fail(
                    E_RECOVERY_INVALID,
                    f"{location}:previous_manifest_hash",
                    "hash_chain_break",
                )
            expected_version -= 1
        return current

    def current_manifest(self) -> dict[str, Any] | None:
        return self.verify_chain()

    def load_chunk_document(
        self,
        record: dict[str, Any],
        verify_hash: bool = True,
    ) -> dict[str, Any]:
        location = f"batch:{record['batch']}"
        path = _safe_relative_path(self.root, record["file"], location)
        try:
            data = path.read_bytes()
        except FileNotFoundError:
            fail(E_RECOVERY_INVALID, location, "missing_chunk", file=record["file"])
        except OSError as exc:
            fail(E_RECOVERY_INVALID, location, "chunk_read_failed", reason=str(exc))
        document, digest = decode_batch(data, location)
        if verify_hash and digest != record["hash"]:
            fail(E_RECOVERY_INVALID, location, "chunk_hash_mismatch")
        return document

    def write_batch(
        self,
        batch: BatchData,
        factor_versions: dict[str, str],
    ) -> dict[str, Any]:
        data, document, digest = encode_batch(batch, factor_versions)
        relative = self.batch_relative(batch.layer, batch.batch_id, digest)
        path = self.root / relative
        try:
            if path.exists():
                existing = path.read_bytes()
                if sha256_bytes(existing) != digest:
                    fail(E_COMMIT_FAILED, f"batch:{batch.batch_id}", "content_address_collision")
            else:
                atomic_write(path, data)
        except PipelineError:
            raise
        except OSError as exc:
            fail(
                E_COMMIT_FAILED,
                f"batch:{batch.batch_id}",
                "chunk_write_failed",
                reason=str(exc),
            )
        return {
            "layer": batch.layer,
            "batch": batch.batch_id,
            "chunk": batch.chunk_id,
            "zone_id": batch.zone_id,
            "file": relative,
            "hash": digest,
            "factor_versions": dict(sorted(factor_versions.items())),
            "stroke_count": len(batch.strokes),
            "bbox": list(batch.bbox),
        }

    def batch_relative(self, layer: int, batch_id: str, digest: str) -> str:
        key_hash = sha256_json({"batch": batch_id})[:16]
        return f"chunks/l{layer}/{key_hash}-{digest[:20]}.nprb"

    def build_manifest(
        self,
        scene: Scene,
        factors: dict[str, str],
        records: list[dict[str, Any]],
        layer_stats: dict[str, Any],
        previous: dict[str, Any] | None,
    ) -> dict[str, Any]:
        version = 1 if previous is None else int(previous["version"]) + 1
        manifest: dict[str, Any] = {
            "format": MANIFEST_FORMAT,
            "schema_version": SCHEMA_VERSION,
            "version": version,
            "scene_id": scene.scene_id,
            "previous_version": None if previous is None else previous["version"],
            "previous_manifest_hash": None if previous is None else previous["manifest_hash"],
            "factors": dict(sorted(factors.items())),
            "layer_stats": normalize(layer_stats),
            "batches": sorted(
                records,
                key=lambda item: (int(item["layer"]), str(item["batch"])),
            ),
        }
        content_hash = sha256_json(manifest_content_payload(manifest))
        manifest["content_hash"] = content_hash
        manifest_hash = sha256_json(
            {key: value for key, value in manifest.items() if key != "manifest_hash"}
        )
        manifest["manifest_hash"] = manifest_hash
        return manifest

    def commit_manifest(self, manifest: dict[str, Any]) -> None:
        version = int(manifest["version"])
        location = self.manifest_relative(version)
        self._validate_manifest_structure(manifest, version, location)
        self.verify_manifest(manifest, location)
        manifest_path = self.manifest_path(version)
        pointer = {
            "format": CURRENT_FORMAT,
            "schema_version": SCHEMA_VERSION,
            "version": version,
            "manifest": location,
            "manifest_hash": manifest["manifest_hash"],
            "content_hash": manifest["content_hash"],
        }
        try:
            atomic_write(manifest_path, canonical_json_bytes(manifest) + b"\n")
            atomic_write(self.current_path, canonical_json_bytes(pointer) + b"\n")
        except OSError as exc:
            fail(E_COMMIT_FAILED, "manifest", "manifest_write_failed", reason=str(exc))

    def write_staging(self, state: dict[str, Any]) -> None:
        state = dict(state)
        state["format"] = STAGING_FORMAT
        state["schema_version"] = SCHEMA_VERSION
        state["state_hash"] = sha256_json(
            {key: value for key, value in state.items() if key != "state_hash"}
        )
        path = self.staging_dir / "state.json"
        try:
            atomic_write(path, canonical_json_bytes(state) + b"\n")
        except OSError as exc:
            fail(E_COMMIT_FAILED, "staging", "staging_write_failed", reason=str(exc))

    def read_staging(self) -> dict[str, Any] | None:
        path = self.staging_dir / "state.json"
        if not path.exists():
            return None
        state = self.read_json_file(path, "staging", E_RECOVERY_INVALID)
        if not isinstance(state, dict) or state.get("format") != STAGING_FORMAT:
            return None
        expected = state.get("state_hash")
        actual = sha256_json(
            {key: value for key, value in state.items() if key != "state_hash"}
        )
        if expected != actual:
            return None
        return state

    def clear_staging(self) -> None:
        path = self.staging_dir / "state.json"
        with contextlib.suppress(FileNotFoundError):
            path.unlink()

    def cleanup_uncommitted(
        self,
        keep_files: set[str] | None = None,
    ) -> list[str]:
        keep_files = set(keep_files or ())
        removed: list[str] = []
        for path in self.root.rglob(".*.tmp.*"):
            if path.is_file():
                with contextlib.suppress(OSError):
                    path.unlink()
                    removed.append(str(path.relative_to(self.root)))
        referenced: set[str] = set(keep_files)
        pointer = self.read_current_pointer()
        if pointer is not None:
            version = pointer["version"]
            while version >= 1:
                manifest = self.read_manifest_file(version)
                for record in manifest.get("batches", []):
                    referenced.add(str(record.get("file")))
                previous = manifest.get("previous_version")
                if previous is None:
                    break
                version = int(previous)
        stale_manifests: set[Path] = set()
        current_chain = (
            {
                self.manifest_path(chain_version)
                for chain_version in range(1, pointer["version"] + 1)
            }
            if pointer is not None
            else set()
        )
        for path in self.manifests_dir.glob("v*.json"):
            if path not in current_chain:
                stale_manifests.add(path)
        for path in sorted(stale_manifests):
            with contextlib.suppress(OSError):
                path.unlink()
                removed.append(str(path.relative_to(self.root)))
        for path in self.chunks_dir.rglob("*.nprb"):
            relative = str(path.relative_to(self.root))
            if relative not in referenced:
                with contextlib.suppress(OSError):
                    path.unlink()
                    removed.append(relative)
        return removed

    def snapshot_version_hash(self) -> str | None:
        pointer = self.read_current_pointer()
        if pointer is None:
            return None
        return str(pointer.get("content_hash"))


__all__ = [
    "CURRENT_FORMAT",
    "MANIFEST_FORMAT",
    "SCHEMA_VERSION",
    "STAGING_FORMAT",
    "Store",
    "atomic_write",
    "compute_manifest_hashes",
    "decode_batch",
    "encode_batch",
    "manifest_content_payload",
]
