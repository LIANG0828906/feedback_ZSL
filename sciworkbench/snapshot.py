"""Atomic per-block snapshots with a verifiable hash chain."""

from __future__ import annotations

import json
import os
import re
import struct
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Optional, Sequence, Tuple

import numpy as np

from .errors import E_RECOVERY_INVALID, WorkbenchError
from .model import SequenceStructure
from .pipeline import BlockResult, StepSummary
from .util import canonical_json_bytes, sha256_bytes, write_bytes_atomic


BLOCK_MAGIC = b"SCIBLK01"
BLOCK_PREFIX = struct.Struct("<8sII")
BLOCK_SCHEMA = 1
MANIFEST_SCHEMA = 1
COMPLETE_SCHEMA = 1
_BLOCK_NAME = re.compile(r"^(\d{6})\.blk$")
_VERSION_NAME = re.compile(r"^v(\d{6})$")


@dataclass(frozen=True)
class RecoveryState:
    version: int
    directory: Path
    manifest: dict
    blocks: Tuple[BlockResult, ...]
    chain_head: str
    complete: bool
    matches_current: bool

    @property
    def committed_count(self) -> int:
        return len(self.blocks)


@dataclass
class SnapshotManifest:
    version: int
    directory: Path
    value: dict
    chain_seed: str
    block_order: Tuple[Tuple[int, int], ...]

    @property
    def total_blocks(self) -> int:
        return len(self.block_order)


class SnapshotStore:
    """One snapshot store per sequence.

    A version directory is published atomically before any block is written.
    Individual blocks then become visible through atomic renames; each one
    commits the previous block digest.  A final ``complete.json`` seals the
    version.  Recovery can therefore continue from any valid prefix.
    """

    def __init__(
        self,
        root: os.PathLike,
        structure: SequenceStructure,
        sequence_hash: str,
        header_hash: str,
    ) -> None:
        self.root = Path(root)
        self.structure = structure
        self.sequence_hash = sequence_hash
        self.header_hash = header_hash
        self.root.mkdir(parents=True, exist_ok=True)
        self.manifest: Optional[SnapshotManifest] = None
        self.chain_head: Optional[str] = None
        self.written_blocks = 0
        self.skipped_blocks = 0

    def version_directories(self) -> List[Tuple[int, Path]]:
        result: List[Tuple[int, Path]] = []
        if not self.root.exists():
            return result
        for child in self.root.iterdir():
            if not child.is_dir():
                continue
            match = _VERSION_NAME.match(child.name)
            if match:
                result.append((int(match.group(1)), child))
        result.sort(key=lambda item: item[0])
        return result

    def create_version(
        self,
        *,
        mode: str,
        factor_hash: str,
        factor_versions: Dict[str, str],
        block_size: int,
    ) -> SnapshotManifest:
        existing = self.version_directories()
        version = (existing[-1][0] + 1) if existing else 1
        core = {
            "schema": MANIFEST_SCHEMA,
            "version": int(version),
            "sequence_hash": self.sequence_hash,
            "header_hash": self.header_hash,
            "mode": mode,
            "factor_hash": factor_hash,
            "factor_versions": dict(factor_versions),
            "block_size": int(block_size),
            "total_blocks": int(self.structure.total_blocks),
            "block_order": [
                [int(block.frame_index), int(block.block_index)]
                for block in self.structure.blocks
            ],
        }
        chain_seed = sha256_bytes(canonical_json_bytes(core))
        value = dict(core)
        value["chain_seed"] = chain_seed
        final_dir = self.root / "v{:06d}".format(version)
        temp_dir = Path(
            tempfile.mkdtemp(
                prefix=".v{:06d}.tmp-".format(version), dir=str(self.root)
            )
        )
        try:
            (temp_dir / "blocks").mkdir()
            write_bytes_atomic(temp_dir / "manifest.json", canonical_json_bytes(value))
            dir_fd = os.open(str(temp_dir), os.O_DIRECTORY)
            try:
                os.fsync(dir_fd)
            finally:
                os.close(dir_fd)
            os.replace(temp_dir, final_dir)
        finally:
            if temp_dir.exists():
                for child in sorted(temp_dir.rglob("*"), reverse=True):
                    if child.is_dir():
                        child.rmdir()
                    else:
                        child.unlink()
                temp_dir.rmdir()
        self.manifest = SnapshotManifest(
            version=version,
            directory=final_dir,
            value=value,
            chain_seed=chain_seed,
            block_order=tuple(
                (int(frame), int(block)) for frame, block in core["block_order"]
            ),
        )
        self.chain_head = chain_seed
        self.written_blocks = 0
        self.skipped_blocks = 0
        return self.manifest

    def _require_manifest(self) -> SnapshotManifest:
        if self.manifest is None:
            raise WorkbenchError(
                E_RECOVERY_INVALID, "no snapshot version has been started"
            )
        return self.manifest

    def commit_block(
        self,
        result: BlockResult,
        *,
        factor_hash: str,
        factor_versions: Dict[str, str],
    ) -> str:
        manifest = self._require_manifest()
        chain_index = (
            result.ordinal
            if result.ordinal < manifest.total_blocks
            else manifest.total_blocks
        )
        if chain_index >= manifest.total_blocks:
            raise WorkbenchError(
                E_RECOVERY_INVALID,
                "snapshot has more blocks than the manifest declares",
                frame=result.frame_index,
                block=result.block_index,
            )
        expected_frame, expected_block = manifest.block_order[chain_index]
        if (result.frame_index, result.block_index) != (
            expected_frame,
            expected_block,
        ):
            raise WorkbenchError(
                E_RECOVERY_INVALID,
                "block does not match the manifest block order",
                frame=expected_frame,
                block=expected_block,
            )
        block_dir = manifest.directory / "blocks"
        block_dir.mkdir(parents=True, exist_ok=True)
        path = block_dir / "{:06d}.blk".format(chain_index)
        if path.exists():
            raise WorkbenchError(
                E_RECOVERY_INVALID,
                "refusing to rewrite an already committed block",
                frame=result.frame_index,
                block=result.block_index,
            )
        if self.chain_head is None:
            raise WorkbenchError(
                E_RECOVERY_INVALID, "snapshot hash-chain head is uninitialised"
            )
        header = {
            "schema": BLOCK_SCHEMA,
            "version": int(manifest.version),
            "chain_index": int(chain_index),
            "sequence_hash": self.sequence_hash,
            "mode": manifest.value["mode"],
            "frame": int(result.frame_index),
            "block": int(result.block_index),
            "shape": [int(result.shape[0]), int(result.shape[1])],
            "factor_hash": factor_hash,
            "factor_versions": dict(factor_versions),
            "prev_hash": self.chain_head,
            "content_hash": result.content_hash,
            "step_summaries": [
                summary.canonical_dict() for summary in result.step_summaries
            ],
            "fixed_float_max_abs": result.fixed_float_max_abs,
            "fixed_float_abs_sum": result.fixed_float_abs_sum,
            "fixed_float_pixel_count": result.fixed_float_pixel_count,
        }
        header_bytes = canonical_json_bytes(header)
        pixel_bytes = result.pixels.tobytes(order="C")
        error_bytes = result.error_bound.tobytes(order="C")
        attribution_bytes = result.attribution.tobytes(order="C")
        content = (
            BLOCK_PREFIX.pack(
                BLOCK_MAGIC, len(header_bytes), int(result.pixel_count)
            )
            + header_bytes
            + pixel_bytes
            + error_bytes
            + attribution_bytes
        )
        digest = sha256_bytes(content)
        fd, temp_name = tempfile.mkstemp(
            prefix=".{:06d}.".format(chain_index),
            suffix=".tmp",
            dir=str(block_dir),
        )
        try:
            with os.fdopen(fd, "wb") as handle:
                handle.write(content)
                handle.write(digest.encode("ascii"))
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp_name, path)
            dir_fd = os.open(str(block_dir), os.O_DIRECTORY)
            try:
                os.fsync(dir_fd)
            finally:
                os.close(dir_fd)
        finally:
            if os.path.exists(temp_name):
                os.unlink(temp_name)
        self.chain_head = digest
        self.written_blocks += 1
        return digest

    def skip_committed(self) -> None:
        self.skipped_blocks += 1

    def finalize(self) -> str:
        manifest = self._require_manifest()
        if self.chain_head is None:
            raise WorkbenchError(
                E_RECOVERY_INVALID, "snapshot hash-chain head is uninitialised"
            )
        value = {
            "schema": COMPLETE_SCHEMA,
            "version": int(manifest.version),
            "chain_head": self.chain_head,
            "total_blocks": int(manifest.total_blocks),
        }
        write_bytes_atomic(
            manifest.directory / "complete.json", canonical_json_bytes(value)
        )
        return self.chain_head

    def _invalid(
        self,
        message: str,
        *,
        frame: Optional[int] = None,
        block: Optional[int] = None,
        path: Optional[Path] = None,
        details: Optional[dict] = None,
    ) -> WorkbenchError:
        return WorkbenchError(
            E_RECOVERY_INVALID,
            message,
            frame=frame,
            block=block,
            path=str(path) if path is not None else None,
            details=details,
        )

    def _load_manifest(
        self, version: int, directory: Path
    ) -> Tuple[dict, str, Tuple[Tuple[int, int], ...]]:
        path = directory / "manifest.json"
        if not path.exists():
            raise self._invalid("snapshot manifest is missing", path=path)
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except Exception as exc:
            raise self._invalid(
                "snapshot manifest is not readable JSON: {}".format(exc), path=path
            ) from exc
        required = {
            "schema",
            "version",
            "sequence_hash",
            "header_hash",
            "mode",
            "factor_hash",
            "factor_versions",
            "block_size",
            "total_blocks",
            "block_order",
            "chain_seed",
        }
        missing = sorted(required.difference(value))
        if missing:
            raise self._invalid(
                "snapshot manifest fields are missing: {}".format(", ".join(missing)),
                path=path,
                details={"missing_fields": missing},
            )
        try:
            if int(value["version"]) != version:
                raise ValueError("version field mismatch")
            if value["schema"] != MANIFEST_SCHEMA:
                raise ValueError("unsupported manifest schema")
            if value["sequence_hash"] != self.sequence_hash:
                raise ValueError("sequence hash mismatch")
            if value["header_hash"] != self.header_hash:
                raise ValueError("header hash mismatch")
            block_order = tuple(
                (int(item[0]), int(item[1])) for item in value["block_order"]
            )
            if len(block_order) != int(value["total_blocks"]):
                raise ValueError("block_order length mismatch")
            if len(block_order) != self.structure.total_blocks:
                raise ValueError("snapshot block count does not match structure")
            core = {
                key: value[key]
                for key in (
                    "schema",
                    "version",
                    "sequence_hash",
                    "header_hash",
                    "mode",
                    "factor_hash",
                    "factor_versions",
                    "block_size",
                    "total_blocks",
                    "block_order",
                )
            }
            expected_seed = sha256_bytes(canonical_json_bytes(core))
            if value["chain_seed"] != expected_seed:
                raise ValueError("manifest chain seed mismatch")
        except (KeyError, TypeError, ValueError, IndexError) as exc:
            raise self._invalid(
                "snapshot manifest is invalid: {}".format(exc), path=path
            ) from exc
        return value, str(value["chain_seed"]), block_order

    def _read_block(
        self,
        path: Path,
        manifest: dict,
        block_order: Sequence[Tuple[int, int]],
        expected_index: int,
        expected_prev: str,
    ) -> Tuple[BlockResult, str]:
        expected_frame, expected_block = block_order[expected_index]
        try:
            raw = path.read_bytes()
        except OSError as exc:
            raise self._invalid(
                "cannot read committed block: {}".format(exc),
                frame=expected_frame,
                block=expected_block,
                path=path,
            ) from exc
        if len(raw) < BLOCK_PREFIX.size + 64:
            raise self._invalid(
                "committed block is truncated",
                frame=expected_frame,
                block=expected_block,
                path=path,
            )
        try:
            magic, header_length, pixel_count = BLOCK_PREFIX.unpack(
                raw[: BLOCK_PREFIX.size]
            )
        except struct.error as exc:
            raise self._invalid(
                "committed block prefix is malformed",
                frame=expected_frame,
                block=expected_block,
                path=path,
            ) from exc
        if magic != BLOCK_MAGIC:
            raise self._invalid(
                "committed block magic mismatch",
                frame=expected_frame,
                block=expected_block,
                path=path,
            )
        header_end = BLOCK_PREFIX.size + header_length
        expected_size = header_end + int(pixel_count) * 17 + 64
        if len(raw) != expected_size:
            raise self._invalid(
                "committed block length does not match its fields",
                frame=expected_frame,
                block=expected_block,
                path=path,
            )
        content = raw[:-64]
        try:
            stored_digest = raw[-64:].decode("ascii", errors="strict")
        except UnicodeDecodeError as exc:
            raise self._invalid(
                "committed block digest is not ASCII",
                frame=expected_frame,
                block=expected_block,
                path=path,
            ) from exc
        actual_digest = sha256_bytes(content)
        if stored_digest != actual_digest:
            raise self._invalid(
                "committed block digest mismatch",
                frame=expected_frame,
                block=expected_block,
                path=path,
            )
        try:
            header = json.loads(
                raw[BLOCK_PREFIX.size : header_end].decode("utf-8")
            )
        except Exception as exc:
            raise self._invalid(
                "committed block header is not readable JSON: {}".format(exc),
                frame=expected_frame,
                block=expected_block,
                path=path,
            ) from exc
        required = {
            "schema",
            "version",
            "chain_index",
            "sequence_hash",
            "mode",
            "frame",
            "block",
            "shape",
            "factor_hash",
            "factor_versions",
            "prev_hash",
            "content_hash",
            "step_summaries",
            "fixed_float_max_abs",
            "fixed_float_abs_sum",
            "fixed_float_pixel_count",
        }
        missing = sorted(required.difference(header))
        if missing:
            raise self._invalid(
                "committed block fields are missing: {}".format(", ".join(missing)),
                frame=expected_frame,
                block=expected_block,
                path=path,
                details={"missing_fields": missing},
            )
        try:
            shape = (int(header["shape"][0]), int(header["shape"][1]))
            if shape[0] * shape[1] != int(pixel_count):
                raise ValueError("pixel count and shape disagree")
            if header["prev_hash"] != expected_prev:
                raise ValueError("hash-chain predecessor mismatch")
            if int(header["chain_index"]) != expected_index:
                raise ValueError("chain index mismatch")
            if int(header["version"]) != int(manifest["version"]):
                raise ValueError("block version mismatch")
            if header["sequence_hash"] != manifest["sequence_hash"]:
                raise ValueError("block sequence hash mismatch")
            if header["factor_hash"] != manifest["factor_hash"]:
                raise ValueError("block factor hash mismatch")
            if header["mode"] != manifest["mode"]:
                raise ValueError("block mode mismatch")
            if (
                int(header["frame"]),
                int(header["block"]),
            ) != (expected_frame, expected_block):
                raise ValueError("block index does not match block order")
        except (KeyError, TypeError, ValueError, IndexError) as exc:
            raise self._invalid(
                "committed block metadata is invalid: {}".format(exc),
                frame=expected_frame,
                block=expected_block,
                path=path,
            ) from exc
        offset = header_end
        count = int(pixel_count)
        pixels = np.frombuffer(raw, dtype="<f8", count=count, offset=offset).reshape(
            shape
        )
        offset += count * 8
        errors = np.frombuffer(raw, dtype="<f8", count=count, offset=offset).reshape(
            shape
        )
        offset += count * 8
        attribution = np.frombuffer(
            raw, dtype=np.int8, count=count, offset=offset
        ).reshape(shape)
        try:
            summaries = tuple(
                StepSummary.from_dict(item) for item in header["step_summaries"]
            )
            factor_versions = dict(header["factor_versions"])
            if not all(
                isinstance(key, str) and isinstance(value, str)
                for key, value in factor_versions.items()
            ):
                raise ValueError("factor_versions must map strings to strings")
            result = BlockResult(
                frame_index=expected_frame,
                block_index=expected_block,
                ordinal=expected_index,
                shape=shape,
                pixels=pixels.copy(),
                error_bound=errors.copy(),
                attribution=attribution.copy(),
                step_summaries=summaries,
                fixed_float_max_abs=float(header["fixed_float_max_abs"]),
                fixed_float_abs_sum=float(header["fixed_float_abs_sum"]),
                fixed_float_pixel_count=int(header["fixed_float_pixel_count"]),
                factor_versions=factor_versions,
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise self._invalid(
                "committed block diagnostic fields are invalid: {}".format(exc),
                frame=expected_frame,
                block=expected_block,
                path=path,
            ) from exc
        if result.content_hash != header["content_hash"]:
            raise self._invalid(
                "committed block content hash mismatch",
                frame=expected_frame,
                block=expected_block,
                path=path,
            )
        return result, actual_digest

    def load_latest(
        self,
        *,
        expected_mode: str,
        expected_factor_hash: str,
    ) -> Optional[RecoveryState]:
        versions = self.version_directories()
        if not versions:
            return None
        version, directory = versions[-1]
        manifest, chain_seed, block_order = self._load_manifest(version, directory)
        block_dir = directory / "blocks"
        if not block_dir.is_dir():
            raise self._invalid(
                "snapshot block directory is missing", path=block_dir
            )
        for child in block_dir.iterdir():
            if not child.is_file() or child.name.startswith("."):
                continue
            if child.suffix == ".blk" and not _BLOCK_NAME.match(child.name):
                raise self._invalid(
                    "snapshot contains a malformed block filename", path=child
                )
        block_paths = sorted(block_dir.glob("*.blk"))
        by_index = {
            int(_BLOCK_NAME.match(path.name).group(1)): path
            for path in block_paths
            if _BLOCK_NAME.match(path.name)
        }
        total = len(block_order)
        for index in by_index:
            if index >= total:
                frame, block = (
                    block_order[-1] if block_order else (-1, -1)
                )
                raise self._invalid(
                    "snapshot contains blocks beyond the declared total",
                    frame=frame,
                    block=block,
                    path=by_index[index],
                )
        loaded: List[BlockResult] = []
        chain_head = chain_seed
        incomplete_at: Optional[int] = None
        for index in range(total):
            path = by_index.get(index)
            if path is None:
                if any(later > index for later in by_index):
                    frame, block = block_order[index]
                    raise self._invalid(
                        "snapshot hash chain has a gap",
                        frame=frame,
                        block=block,
                        details={"missing_chain_index": index},
                    )
                incomplete_at = index
                break
            result, chain_head = self._read_block(
                path, manifest, block_order, index, chain_head
            )
            loaded.append(result)
        complete_path = directory / "complete.json"
        complete = False
        if complete_path.exists():
            try:
                complete_value = json.loads(
                    complete_path.read_text(encoding="utf-8")
                )
                for field in ("schema", "version", "chain_head", "total_blocks"):
                    if field not in complete_value:
                        raise ValueError("missing field {}".format(field))
                if complete_value["schema"] != COMPLETE_SCHEMA:
                    raise ValueError("unsupported complete schema")
                if int(complete_value["version"]) != version:
                    raise ValueError("complete version mismatch")
                if int(complete_value["total_blocks"]) != total:
                    raise ValueError("complete block count mismatch")
                if incomplete_at is not None:
                    frame, block = block_order[incomplete_at]
                    raise self._invalid(
                        "snapshot is marked complete but a block is missing",
                        frame=frame,
                        block=block,
                        path=complete_path,
                    )
                if complete_value["chain_head"] != chain_head:
                    frame, block = block_order[-1]
                    raise self._invalid(
                        "complete marker chain head mismatch",
                        frame=frame,
                        block=block,
                        path=complete_path,
                    )
                complete = True
            except WorkbenchError:
                raise
            except Exception as exc:
                frame, block = block_order[min(len(loaded), total - 1)]
                raise self._invalid(
                    "snapshot complete marker is invalid: {}".format(exc),
                    frame=frame,
                    block=block,
                    path=complete_path,
                ) from exc
        matches = (
            manifest["mode"] == expected_mode
            and manifest["factor_hash"] == expected_factor_hash
        )
        self.manifest = SnapshotManifest(
            version=version,
            directory=directory,
            value=manifest,
            chain_seed=chain_seed,
            block_order=block_order,
        )
        self.chain_head = chain_head
        return RecoveryState(
            version=version,
            directory=directory,
            manifest=manifest,
            blocks=tuple(loaded),
            chain_head=chain_head,
            complete=complete,
            matches_current=matches,
        )
