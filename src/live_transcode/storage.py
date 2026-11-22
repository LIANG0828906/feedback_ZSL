from __future__ import annotations

import contextlib
import copy
import fcntl
import hashlib
import json
import os
import shutil
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

from .errors import (
    E_BUSY,
    E_INVALID_ARGUMENT,
    E_NOT_FOUND,
    TaskError,
    recovery_error,
    version_error,
)
from .models import TierParams, canonical_json_bytes, sha256_bytes
from .sidecar import compute_tier_version_hash, sidecar_bytes

MANIFEST_SCHEMA = "live-transcode-manifest/v1"
SNAPSHOT_SCHEMA = "live-transcode-snapshot/v1"
CATALOG_SCHEMA = "live-transcode-catalog/v1"


def _fsync_dir(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}-{uuid.uuid4().hex}")
    try:
        with open(temporary, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        _fsync_dir(path.parent)
    finally:
        with contextlib.suppress(FileNotFoundError):
            temporary.unlink()


def atomic_write_json(path: Path, value: Any) -> None:
    atomic_write_bytes(path, canonical_json_bytes(value) + b"\n")


def read_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise TaskError(E_NOT_FOUND, f"file not found: {path.name}", field=str(path)) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise TaskError(
            E_INVALID_ARGUMENT,
            f"cannot read JSON {path.name}: {exc}",
            field=str(path),
        ) from exc


@contextlib.contextmanager
def file_lock(path: Path) -> Iterator[None]:
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "a+b") as handle:
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


@contextlib.contextmanager
def try_file_lock(path: Path, *, tier_id: str) -> Iterator[None]:
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "a+b") as handle:
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as exc:
            raise TaskError(
                E_BUSY,
                f"tier is already being processed: {tier_id}",
                field="tier_id",
                details={"tier_id": tier_id},
            ) from exc
        try:
            yield
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


@dataclass(frozen=True)
class PublishedTier:
    tier_id: str
    generation: int
    generation_hash: str
    manifest_hash: str
    tier_version_hash: str
    param_version: int
    sidecar: dict[str, Any]
    audio_slices: list[bytes]
    run_dir: Path


class TierCatalog:
    def __init__(self, store_root: str | os.PathLike[str]):
        self.root = Path(store_root)
        self.root.mkdir(parents=True, exist_ok=True)
        self.path = self.root / "catalog.json"
        self.lock_path = self.root / "locks" / "catalog.lock"

    def _load_unlocked(self) -> dict[str, Any]:
        if not self.path.exists():
            return {"schema": CATALOG_SCHEMA, "tiers": {}}
        data = read_json(self.path)
        if data.get("schema") != CATALOG_SCHEMA:
            raise TaskError(
                E_INVALID_ARGUMENT,
                "catalog schema mismatch",
                field="catalog.schema",
                details={"value": data.get("schema")},
            )
        return data

    def list_tiers(self) -> list[TierParams]:
        data = self._load_unlocked()
        return [TierParams(**item) for _, item in sorted(data["tiers"].items())]

    def get(self, tier_id: str) -> TierParams | None:
        data = self._load_unlocked()
        item = data["tiers"].get(tier_id)
        return TierParams(**item) if item else None

    def configure(
        self,
        tier_id: str,
        target_rate: int,
        filter_length: int,
        quant_bits: int,
        slice_samples: int = 2048,
    ) -> TierParams:
        with file_lock(self.lock_path):
            data = self._load_unlocked()
            old_item = data["tiers"].get(tier_id)
            if old_item:
                old = TierParams(**old_item)
                if (
                    old.target_rate == target_rate
                    and old.filter_length == filter_length
                    and old.quant_bits == quant_bits
                    and old.slice_samples == slice_samples
                ):
                    return old
                new_version = old.param_version + 1
            else:
                new_version = 1
            tier = TierParams(
                tier_id=tier_id,
                target_rate=target_rate,
                filter_length=filter_length,
                quant_bits=quant_bits,
                param_version=new_version,
                slice_samples=slice_samples,
            )
            data["tiers"][tier_id] = tier.as_dict()
            atomic_write_json(self.path, data)
            return tier


class VersionStore:
    def __init__(self, root: str | os.PathLike[str]):
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)
        self.runs_root = self.root / "runs"
        self.locks_root = self.root / "locks"
        self.manifest_path = self.root / "published.json"
        self.manifest_lock = self.locks_root / "published.lock"
        self.runs_root.mkdir(parents=True, exist_ok=True)
        self.locks_root.mkdir(parents=True, exist_ok=True)

    def run_dir(self, tier_id: str, run_id: str) -> Path:
        return self.runs_root / tier_id / run_id

    def run_lock_path(self, tier_id: str) -> Path:
        return self.locks_root / f"tier-{tier_id}.lock"

    @contextlib.contextmanager
    def run_lock(self, tier_id: str) -> Iterator[None]:
        with file_lock(self.run_lock_path(tier_id)):
            yield

    def write_snapshot(self, run_dir: Path, snapshot: dict[str, Any]) -> None:
        run_dir.mkdir(parents=True, exist_ok=True)
        atomic_write_json(run_dir / "snapshot.json", snapshot)

    def load_snapshot(self, run_dir: Path) -> dict[str, Any]:
        path = run_dir / "snapshot.json"
        data = read_json(path)
        if data.get("schema") != SNAPSHOT_SCHEMA:
            raise recovery_error(
                "snapshot schema mismatch",
                details={"schema": data.get("schema")},
            )
        return data

    def commit_slice(
        self,
        run_dir: Path,
        index: int,
        pcm: bytes,
        meta: dict[str, Any],
        *,
        audio_source: Path | None = None,
    ) -> None:
        slices_dir = run_dir / "slices"
        slices_dir.mkdir(parents=True, exist_ok=True)
        final_dir = slices_dir / f"{index:06d}"
        if final_dir.exists():
            raise recovery_error("refusing to overwrite a committed slice", index)
        temp_dir = slices_dir / f".tmp-{index:06d}-{os.getpid()}-{uuid.uuid4().hex}"
        temp_dir.mkdir()
        try:
            audio_path = temp_dir / "audio.pcm"
            if audio_source is not None:
                try:
                    os.link(audio_source, audio_path)
                except OSError:
                    shutil.copyfile(audio_source, audio_path)
            else:
                with open(audio_path, "wb") as handle:
                    handle.write(pcm)
                    handle.flush()
                    os.fsync(handle.fileno())
            atomic_write_json(temp_dir / "meta.json", meta)
            _fsync_dir(temp_dir)
            os.replace(temp_dir, final_dir)
            _fsync_dir(slices_dir)
        finally:
            if temp_dir.exists():
                shutil.rmtree(temp_dir, ignore_errors=True)

    def slice_dir(self, run_dir: Path, index: int) -> Path:
        return run_dir / "slices" / f"{index:06d}"

    def read_slice_audio(self, run_dir: Path, index: int) -> bytes:
        path = self.slice_dir(run_dir, index) / "audio.pcm"
        try:
            return path.read_bytes()
        except FileNotFoundError as exc:
            raise recovery_error("committed audio slice is missing", index) from exc

    def read_slice_meta(self, run_dir: Path, index: int) -> dict[str, Any]:
        path = self.slice_dir(run_dir, index) / "meta.json"
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError) as exc:
            raise recovery_error("committed sidecar slice is missing or invalid", index) from exc

    def list_committed_indices(self, run_dir: Path) -> list[int]:
        slices_dir = run_dir / "slices"
        if not slices_dir.exists():
            return []
        indices: list[int] = []
        for child in slices_dir.iterdir():
            if child.is_dir() and len(child.name) == 6 and child.name.isdigit():
                indices.append(int(child.name))
        return sorted(indices)

    def finalize_run(
        self,
        run_dir: Path,
        sidecar: dict[str, Any],
        version_descriptor: dict[str, Any],
    ) -> None:
        sidecar_path = run_dir / "sidecar.json"
        atomic_write_bytes(sidecar_path, sidecar_bytes(sidecar))
        atomic_write_json(run_dir / "version.json", version_descriptor)
        atomic_write_bytes(run_dir / ".complete", b"complete\n")

    def is_complete(self, run_dir: Path) -> bool:
        return (run_dir / ".complete").is_file()

    def load_run_sidecar(self, run_dir: Path) -> dict[str, Any]:
        path = run_dir / "sidecar.json"
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError) as exc:
            raise recovery_error("run sidecar is missing or invalid") from exc

    def load_run_version(self, run_dir: Path) -> dict[str, Any]:
        path = run_dir / "version.json"
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError) as exc:
            raise recovery_error("run version descriptor is missing or invalid") from exc

    @staticmethod
    def _compute_generation_hash(
        generation: int, tiers: dict[str, Any]
    ) -> str:
        payload = {
            "schema": MANIFEST_SCHEMA,
            "generation": generation,
            "tiers": tiers,
        }
        return sha256_bytes(canonical_json_bytes(payload))

    def _load_manifest_unlocked(self) -> dict[str, Any]:
        if not self.manifest_path.exists():
            return {
                "schema": MANIFEST_SCHEMA,
                "generation": 0,
                "generation_hash": self._compute_generation_hash(0, {}),
                "tiers": {},
            }
        try:
            data = json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise version_error("published manifest is unreadable", field="published.json") from exc
        if data.get("schema") != MANIFEST_SCHEMA:
            raise version_error("published manifest schema mismatch", field="published.schema")
        expected = self._compute_generation_hash(
            int(data.get("generation", -1)), data.get("tiers", {})
        )
        if data.get("generation_hash") != expected:
            raise version_error(
                "published manifest generation hash does not match its contents",
                field="published.generation_hash",
                declared=data.get("generation_hash"),
                expected=expected,
            )
        return data

    def publish_tier(
        self,
        *,
        tier: TierParams,
        run_id: str,
        run_dir: Path,
        tier_version_hash: str,
        sidecar_path_digest: str,
        material_id: str,
        source_hash: str,
    ) -> dict[str, Any]:
        if not self.is_complete(run_dir):
            raise version_error("cannot publish an incomplete tier run", field="run.complete")
        with file_lock(self.manifest_lock):
            manifest = self._load_manifest_unlocked()
            tiers = copy.deepcopy(manifest.get("tiers", {}))
            descriptor = {
                "run_id": run_id,
                "tier_version_hash": tier_version_hash,
                "sidecar_digest": sidecar_path_digest,
                "param_version": tier.param_version,
                "params": tier.as_dict(),
                "material_id": material_id,
                "source_hash": source_hash,
            }
            if tiers.get(tier.tier_id) == descriptor:
                return manifest
            tiers[tier.tier_id] = descriptor
            generation = int(manifest.get("generation", 0)) + 1
            generation_hash = self._compute_generation_hash(generation, tiers)
            published = {
                "schema": MANIFEST_SCHEMA,
                "generation": generation,
                "generation_hash": generation_hash,
                "tiers": tiers,
            }
            atomic_write_json(self.manifest_path, published)
            return published

    def read_manifest(self) -> dict[str, Any]:
        return self._load_manifest_unlocked()

    def query_tier(
        self,
        tier_id: str,
        *,
        expected_manifest_hash: str | None = None,
    ) -> PublishedTier:
        try:
            raw_manifest = self.manifest_path.read_bytes()
        except FileNotFoundError as exc:
            raise TaskError(E_NOT_FOUND, "no published generation exists", field="published.json") from exc
        try:
            manifest = json.loads(raw_manifest)
        except json.JSONDecodeError as exc:
            raise version_error("published manifest is invalid JSON", field="published.json") from exc
        if manifest.get("schema") != MANIFEST_SCHEMA:
            raise version_error("published manifest schema mismatch", field="published.schema")
        expected = self._compute_generation_hash(
            int(manifest.get("generation", -1)), manifest.get("tiers", {})
        )
        if manifest.get("generation_hash") != expected:
            raise version_error(
                "published manifest generation hash does not match its contents",
                field="published.generation_hash",
                declared=manifest.get("generation_hash"),
                expected=expected,
            )
        manifest_hash = sha256_bytes(raw_manifest)
        if expected_manifest_hash and expected_manifest_hash not in (
            manifest.get("generation_hash"),
            manifest_hash,
        ):
            raise version_error(
                "requested manifest hash is stale",
                field="expected_manifest_hash",
                expected=expected_manifest_hash,
                actual_generation_hash=manifest.get("generation_hash"),
                actual_manifest_hash=manifest_hash,
            )
        descriptor = manifest.get("tiers", {}).get(tier_id)
        if not descriptor:
            raise TaskError(
                E_NOT_FOUND,
                f"tier is not published: {tier_id}",
                field="tier_id",
            )
        run_dir = self.run_dir(tier_id, descriptor["run_id"])
        if not self.is_complete(run_dir):
            raise version_error(
                "manifest points at a tier version without a completion marker",
                field="run.complete",
            )
        sidecar_path = run_dir / "sidecar.json"
        try:
            sidecar_raw = sidecar_path.read_bytes()
            sidecar = json.loads(sidecar_raw)
        except (FileNotFoundError, json.JSONDecodeError) as exc:
            raise version_error("published sidecar is missing or invalid", field="sidecar") from exc
        if sha256_bytes(sidecar_raw) != descriptor.get("sidecar_digest"):
            raise version_error(
                "sidecar digest does not match the published manifest",
                field="tiers.sidecar_digest",
                tier_id=tier_id,
            )
        computed_version_hash = compute_tier_version_hash(sidecar)
        if sidecar.get("tier_version_hash") != computed_version_hash:
            raise version_error(
                "sidecar self hash does not match its contents",
                field="sidecar.tier_version_hash",
                tier_id=tier_id,
            )
        if computed_version_hash != descriptor.get("tier_version_hash"):
            raise version_error(
                "tier version hash does not match the published manifest",
                field="tiers.tier_version_hash",
                tier_id=tier_id,
                declared=descriptor.get("tier_version_hash"),
                computed=computed_version_hash,
            )
        if descriptor.get("param_version") != sidecar.get("param_version"):
            raise version_error(
                "parameter version does not match the published manifest",
                field="tiers.param_version",
                tier_id=tier_id,
            )
        audio: list[bytes] = []
        for index in range(int(sidecar.get("total_slices", 0))):
            pcm = self.read_slice_audio(run_dir, index)
            meta = self.read_slice_meta(run_dir, index)
            if meta.get("content_hash") != sha256_bytes(pcm):
                raise version_error(
                    "immutable audio buffer hash does not match slice sidecar",
                    field="slice.content_hash",
                    tier_id=tier_id,
                    slice_index=index,
                )
            audio.append(pcm)
        return PublishedTier(
            tier_id=tier_id,
            generation=int(manifest["generation"]),
            generation_hash=manifest["generation_hash"],
            manifest_hash=manifest_hash,
            tier_version_hash=computed_version_hash,
            param_version=int(descriptor["param_version"]),
            sidecar=sidecar,
            audio_slices=audio,
            run_dir=run_dir,
        )

    def find_run_dirs(self, tier_id: str) -> list[Path]:
        root = self.runs_root / tier_id
        if not root.exists():
            return []
        return sorted(path for path in root.iterdir() if path.is_dir())

