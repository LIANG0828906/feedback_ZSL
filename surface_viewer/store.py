from __future__ import annotations

import json
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Mapping

from .artifacts import Artifact
from .errors import ErrorCode, SurfaceViewerError
from .graph import DependencyGraph
from .hashing import canonical_json_bytes, sha256_bytes, sha256_json
from .model import SurfaceAssembly

MANIFEST_VERSION = "surface-viewer-manifest-v1"
MANIFEST_SCHEMA_VERSION = 1
ZERO_HASH = "0" * 64
HASH_PATTERN = re.compile(r"^[0-9a-f]{64}$")


@dataclass(frozen=True)
class RecoveryState:
    commit_seq: int
    assembly_version: str | None
    artifacts: dict[str, Artifact]
    chain_head: str
    history: tuple[dict[str, object], ...]
    manifest: dict[str, object] | None


@dataclass(frozen=True)
class CommitInfo:
    commit_seq: int
    chain_head: str
    previous_chain_head: str
    artifact_hashes: dict[str, str]
    manifest_path: str


class ArtifactStore:
    def __init__(self, root: str | Path):
        self.root = Path(root)
        self.artifacts_dir = self.root / "artifacts"
        self.temp_dir = self.root / "tmp"
        self.manifest_path = self.root / "manifest.json"
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)
        self.temp_dir.mkdir(parents=True, exist_ok=True)

    def recover(self, *, expected_assembly_id: str | None = None) -> RecoveryState:
        if not self.manifest_path.exists():
            return RecoveryState(
                commit_seq=0,
                assembly_version=None,
                artifacts={},
                chain_head=ZERO_HASH,
                history=(),
                manifest=None,
            )
        try:
            manifest = json.loads(
                self.manifest_path.read_text(encoding="utf-8")
            )
        except (OSError, json.JSONDecodeError) as exc:
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "committed manifest cannot be parsed",
                field="manifest.json",
                details={"error": str(exc)},
            )
        self._validate_manifest(manifest, expected_assembly_id)

        artifacts: dict[str, Artifact] = {}
        for entry in manifest["artifacts"]:
            content_hash = entry["content_hash"]
            blob_path = self.artifacts_dir / f"{content_hash}.bin"
            try:
                data = blob_path.read_bytes()
            except OSError as exc:
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "committed artifact blob is missing",
                    field=entry["artifact_id"],
                    details={"path": str(blob_path), "error": str(exc)},
                )
            actual_hash = sha256_bytes(data)
            if actual_hash != content_hash:
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "committed artifact hash does not match blob content",
                    field=entry["artifact_id"],
                    details={
                        "expected_hash": content_hash,
                        "actual_hash": actual_hash,
                    },
                )
            artifact = Artifact(
                artifact_id=entry["artifact_id"],
                patch_id=entry["patch_id"],
                kind=entry["kind"],
                data=data,
                content_hash=content_hash,
                parameter_signature=entry["parameter_signature"],
            )
            artifacts[artifact.artifact_id] = artifact

        return RecoveryState(
            commit_seq=manifest["commit_seq"],
            assembly_version=manifest["assembly_version"],
            artifacts=artifacts,
            chain_head=manifest["chain_head"],
            history=tuple(manifest["history"]),
            manifest=manifest,
        )

    def _validate_manifest(
        self, manifest: object, expected_assembly_id: str | None
    ) -> None:
        if not isinstance(manifest, dict):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest root must be an object",
                field="manifest.json",
            )
        required = (
            "manifest_version",
            "schema_version",
            "assembly_id",
            "assembly_version",
            "patch_versions",
            "parameter_versions",
            "parameter_versions_hash",
            "commit_seq",
            "previous_chain_head",
            "chain_head",
            "artifacts",
            "history",
        )
        for field in required:
            if field not in manifest:
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "manifest required field is missing",
                    field=field,
                )
        if manifest["manifest_version"] != MANIFEST_VERSION:
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest version is not supported",
                field="manifest_version",
                details={
                    "expected": MANIFEST_VERSION,
                    "actual": manifest["manifest_version"],
                },
            )
        if manifest["schema_version"] != MANIFEST_SCHEMA_VERSION:
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest schema version is not supported",
                field="schema_version",
                details={
                    "expected": MANIFEST_SCHEMA_VERSION,
                    "actual": manifest["schema_version"],
                },
            )
        if expected_assembly_id is not None and (
            manifest["assembly_id"] != expected_assembly_id
        ):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest belongs to a different assembly",
                field="assembly_id",
                details={
                    "expected": expected_assembly_id,
                    "actual": manifest["assembly_id"],
                },
            )
        if (
            not isinstance(manifest["commit_seq"], int)
            or isinstance(manifest["commit_seq"], bool)
            or manifest["commit_seq"] < 1
        ):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest commit sequence is invalid",
                field="commit_seq",
            )
        for field in (
            "assembly_version",
            "parameter_versions_hash",
            "previous_chain_head",
            "chain_head",
        ):
            if not isinstance(manifest[field], str) or not HASH_PATTERN.fullmatch(
                manifest[field]
            ):
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "manifest hash field is invalid",
                    field=field,
                )
        if not isinstance(manifest["patch_versions"], dict):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest patch_versions must be an object",
                field="patch_versions",
            )
        if not isinstance(manifest["parameter_versions"], dict):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest parameter_versions must be an object",
                field="parameter_versions",
            )
        if set(manifest["patch_versions"]) != set(
            manifest["parameter_versions"]
        ):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest patch and parameter version maps differ",
                field="parameter_versions",
            )
        for patch_id, patch_version in manifest["patch_versions"].items():
            if not isinstance(patch_id, str) or not isinstance(
                patch_version, str
            ) or not HASH_PATTERN.fullmatch(patch_version):
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "manifest patch version is invalid",
                    field=f"patch_versions.{patch_id}",
                )
            parameter_versions = manifest["parameter_versions"][patch_id]
            if not isinstance(parameter_versions, dict):
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "manifest parameter version set must be an object",
                    field=f"parameter_versions.{patch_id}",
                )
            for name, version in parameter_versions.items():
                if not isinstance(name, str) or not isinstance(
                    version, str
                ) or not HASH_PATTERN.fullmatch(version):
                    raise SurfaceViewerError(
                        ErrorCode.RECOVERY_INVALID,
                        "manifest parameter version is invalid",
                        field=f"parameter_versions.{patch_id}.{name}",
                    )
        if manifest["parameter_versions_hash"] != sha256_json(
            manifest["parameter_versions"]
        ):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest parameter version hash is invalid",
                field="parameter_versions_hash",
            )
        if not isinstance(manifest["artifacts"], list):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest artifacts must be an array",
                field="artifacts",
            )
        if not isinstance(manifest["history"], list):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest history must be an array",
                field="history",
            )

        seen_ids: set[str] = set()
        chain = manifest["previous_chain_head"]
        for index, entry in enumerate(manifest["artifacts"]):
            field_prefix = f"artifacts[{index}]"
            if not isinstance(entry, dict):
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "manifest artifact entry must be an object",
                    field=field_prefix,
                )
            for field in (
                "artifact_id",
                "patch_id",
                "kind",
                "content_hash",
                "parameter_signature",
                "dependency_versions",
                "chain_hash",
            ):
                if field not in entry:
                    raise SurfaceViewerError(
                        ErrorCode.RECOVERY_INVALID,
                        "manifest artifact field is missing",
                        field=f"{field_prefix}.{field}",
                    )
            if entry["artifact_id"] in seen_ids:
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "manifest contains a duplicate artifact",
                    field=f"{field_prefix}.artifact_id",
                )
            seen_ids.add(entry["artifact_id"])
            if not HASH_PATTERN.fullmatch(entry["content_hash"]):
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "artifact content hash is invalid",
                    field=f"{field_prefix}.content_hash",
                )
            expected_chain = sha256_json(
                {
                    "previous": chain,
                    "assembly_version": manifest["assembly_version"],
                    "parameter_versions_hash": manifest[
                        "parameter_versions_hash"
                    ],
                    "artifact": {
                        key: value for key, value in entry.items() if key != "chain_hash"
                    },
                }
            )
            if entry["chain_hash"] != expected_chain:
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "artifact hash chain is broken",
                    field=f"{field_prefix}.chain_hash",
                    details={
                        "expected": expected_chain,
                        "actual": entry["chain_hash"],
                    },
                )
            chain = expected_chain
        if chain != manifest["chain_head"]:
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest chain head does not match artifact chain",
                field="chain_head",
            )

        previous = ZERO_HASH
        for index, record in enumerate(manifest["history"]):
            field_prefix = f"history[{index}]"
            if not isinstance(record, dict):
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "manifest history entry must be an object",
                    field=field_prefix,
                )
            if record.get("previous_chain_head") != previous:
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "commit history hash chain is broken",
                    field=f"{field_prefix}.previous_chain_head",
                )
            previous = record.get("chain_head")
            if not isinstance(previous, str) or not HASH_PATTERN.fullmatch(previous):
                raise SurfaceViewerError(
                    ErrorCode.RECOVERY_INVALID,
                    "commit history chain head is invalid",
                    field=f"{field_prefix}.chain_head",
                )
        if not manifest["history"]:
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest history is empty",
                field="history",
            )
        if previous != manifest["chain_head"]:
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest history does not end at the current chain head",
                field="history",
            )
        if manifest["history"][-1].get("commit_seq") != manifest["commit_seq"]:
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "manifest commit sequence differs from history",
                field="commit_seq",
            )

    def commit(
        self,
        assembly: SurfaceAssembly,
        graph: DependencyGraph,
        artifacts: Mapping[str, Artifact],
        *,
        expected_previous_commit_seq: int | None = None,
        crash_after_artifact_writes: int | None = None,
        after_artifact_replace: Callable[[int], None] | None = None,
    ) -> CommitInfo:
        graph.assert_publishable()
        expected_ids = {node.node_id for node in graph.artifact_nodes()}
        actual_ids = set(artifacts)
        if expected_ids != actual_ids:
            raise SurfaceViewerError(
                ErrorCode.DEP_GRAPH_INCOMPLETE,
                "artifact set does not match dependency graph",
                details={
                    "missing_artifacts": sorted(expected_ids - actual_ids),
                    "extra_artifacts": sorted(actual_ids - expected_ids),
                },
            )

        previous = self.recover(expected_assembly_id=assembly.assembly_id)
        if (
            expected_previous_commit_seq is not None
            and previous.commit_seq != expected_previous_commit_seq
        ):
            raise SurfaceViewerError(
                ErrorCode.RECOVERY_INVALID,
                "store commit sequence changed while recomputing",
                field="commit_seq",
                details={
                    "expected": expected_previous_commit_seq,
                    "actual": previous.commit_seq,
                },
            )

        next_sequence = previous.commit_seq + 1
        for temporary in self.temp_dir.glob("commit-*.tmp"):
            try:
                temporary.unlink()
            except OSError:
                pass

        entries: list[dict[str, object]] = []
        for write_index, artifact_id in enumerate(sorted(artifacts)):
            artifact = artifacts[artifact_id]
            temporary = self.temp_dir / f"commit-{next_sequence:08d}-{write_index:08d}.tmp"
            with temporary.open("wb") as stream:
                stream.write(artifact.data)
                stream.flush()
                os.fsync(stream.fileno())
            final_path = self.artifacts_dir / f"{artifact.content_hash}.bin"
            os.replace(temporary, final_path)
            if after_artifact_replace is not None:
                after_artifact_replace(write_index + 1)
            if (
                crash_after_artifact_writes is not None
                and write_index + 1 >= crash_after_artifact_writes
            ):
                os._exit(23)
            entries.append(
                artifact.to_manifest_entry(
                    graph.dependency_versions(artifact.artifact_id)
                )
            )
        _fsync_directory(self.artifacts_dir)

        previous_chain = previous.chain_head
        chain = previous_chain
        parameter_versions = {
            patch.patch_id: patch.parameter_versions
            for patch in sorted(
                assembly.patches, key=lambda item: item.patch_id
            )
        }
        parameter_versions_hash = sha256_json(parameter_versions)
        for entry in entries:
            chain = sha256_json(
                {
                    "previous": chain,
                    "assembly_version": assembly.version_hash,
                    "parameter_versions_hash": parameter_versions_hash,
                    "artifact": entry,
                }
            )
            entry["chain_hash"] = chain

        history = list(previous.history)
        history.append(
            {
                "commit_seq": next_sequence,
                "assembly_version": assembly.version_hash,
                "previous_chain_head": previous_chain,
                "chain_head": chain,
            }
        )
        manifest = {
            "manifest_version": MANIFEST_VERSION,
            "schema_version": MANIFEST_SCHEMA_VERSION,
            "assembly_id": assembly.assembly_id,
            "assembly_version": assembly.version_hash,
            "patch_versions": {
                patch.patch_id: patch.version_hash
                for patch in sorted(
                    assembly.patches, key=lambda item: item.patch_id
                )
            },
            "parameter_versions": parameter_versions,
            "parameter_versions_hash": parameter_versions_hash,
            "commit_seq": next_sequence,
            "previous_chain_head": previous_chain,
            "chain_head": chain,
            "artifacts": entries,
            "history": history,
        }
        temporary_manifest = self.temp_dir / "manifest.tmp"
        with temporary_manifest.open("wb") as stream:
            stream.write(canonical_json_bytes(manifest) + b"\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary_manifest, self.manifest_path)
        _fsync_directory(self.root)
        return CommitInfo(
            commit_seq=next_sequence,
            chain_head=chain,
            previous_chain_head=previous_chain,
            artifact_hashes={
                artifact_id: artifacts[artifact_id].content_hash
                for artifact_id in sorted(artifacts)
            },
            manifest_path=str(self.manifest_path),
        )


def _fsync_directory(path: Path) -> None:
    if os.name == "nt":
        return
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
