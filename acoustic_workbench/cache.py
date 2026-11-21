"""Content-addressed node cache with atomic publication."""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Mapping, Optional, Tuple

import numpy as np

from .util import (
    array_bytes,
    array_from_bytes,
    atomic_write_bytes,
    atomic_write_json,
    canonical_json_bytes,
    hash_obj,
    read_json,
    sha256_bytes,
    sha256_file,
)


@dataclass
class NodeResult:
    stage: str
    fingerprint: str
    summary: Dict[str, Any]
    arrays: Dict[str, np.ndarray]
    inputs: Dict[str, str]
    params: Dict[str, Any]
    result_hash: str
    cache_hit: bool = False


class ContentCache:
    def __init__(self, root: str | os.PathLike[str]) -> None:
        self.root = Path(root)
        self.blobs = self.root / "blobs"
        self.nodes = self.root / "nodes"
        self.blobs.mkdir(parents=True, exist_ok=True)
        self.nodes.mkdir(parents=True, exist_ok=True)

    def blob_path(self, digest: str) -> Path:
        return self.blobs / digest[:2] / f"{digest}.npy"

    def put_blob(self, values: np.ndarray) -> Tuple[str, Dict[str, Any]]:
        raw = array_bytes(values)
        digest = sha256_bytes(raw)
        path = self.blob_path(digest)
        if not path.exists():
            atomic_write_bytes(path, raw)
        return digest, {
            "hash": digest,
            "shape": [int(item) for item in values.shape],
            "dtype": str(values.dtype),
        }

    def get_blob(self, digest: str) -> np.ndarray:
        path = self.blob_path(digest)
        raw = path.read_bytes()
        if sha256_bytes(raw) != digest:
            raise ValueError(f"Blob hash mismatch: {digest}")
        return array_from_bytes(raw)

    def node_path(self, fingerprint: str) -> Path:
        return self.nodes / f"{fingerprint}.json"

    def put_node(
        self,
        *,
        stage: str,
        fingerprint: str,
        inputs: Mapping[str, str],
        params: Mapping[str, Any],
        summary: Mapping[str, Any],
        arrays: Mapping[str, np.ndarray],
    ) -> NodeResult:
        array_refs: Dict[str, Any] = {}
        for name in sorted(arrays):
            digest, ref = self.put_blob(np.asarray(arrays[name]))
            array_refs[name] = ref
        canonical_summary = dict(summary)
        result_hash = hash_obj(
            {
                "stage": stage,
                "inputs": dict(inputs),
                "params": dict(params),
                "summary": canonical_summary,
                "arrays": array_refs,
            }
        )
        meta = {
            "schema_version": 1,
            "stage": stage,
            "fingerprint": fingerprint,
            "inputs": dict(inputs),
            "params": dict(params),
            "summary": canonical_summary,
            "arrays": array_refs,
            "result_hash": result_hash,
        }
        path = self.node_path(fingerprint)
        if not path.exists():
            atomic_write_json(path, meta)
        return NodeResult(
            stage=stage,
            fingerprint=fingerprint,
            summary=canonical_summary,
            arrays={name: np.asarray(value) for name, value in arrays.items()},
            inputs=dict(inputs),
            params=dict(params),
            result_hash=result_hash,
            cache_hit=False,
        )

    def get_node(self, stage: str, fingerprint: str) -> Optional[NodeResult]:
        path = self.node_path(fingerprint)
        if not path.exists():
            return None
        try:
            meta = read_json(path)
            if (
                meta.get("schema_version") != 1
                or meta.get("stage") != stage
                or meta.get("fingerprint") != fingerprint
            ):
                return None
            arrays: Dict[str, np.ndarray] = {}
            for name in sorted(meta.get("arrays", {})):
                ref = meta["arrays"][name]
                digest = ref["hash"]
                values = self.get_blob(digest)
                if list(values.shape) != list(ref["shape"]):
                    return None
                arrays[name] = values
            expected_hash = hash_obj(
                {
                    "stage": meta["stage"],
                    "inputs": meta["inputs"],
                    "params": meta["params"],
                    "summary": meta["summary"],
                    "arrays": meta["arrays"],
                }
            )
            if expected_hash != meta.get("result_hash"):
                return None
            return NodeResult(
                stage=stage,
                fingerprint=fingerprint,
                summary=dict(meta["summary"]),
                arrays=arrays,
                inputs=dict(meta["inputs"]),
                params=dict(meta["params"]),
                result_hash=meta["result_hash"],
                cache_hit=True,
            )
        except (OSError, ValueError, KeyError, TypeError):
            return None

    def get_node_meta(self, stage: str, fingerprint: str) -> Optional[Dict[str, Any]]:
        path = self.node_path(fingerprint)
        if not path.exists():
            return None
        try:
            meta = read_json(path)
        except (OSError, ValueError):
            return None
        if meta.get("stage") != stage or meta.get("fingerprint") != fingerprint:
            return None
        return meta

    def published_path(self, listener_id: str) -> Path:
        return self.root / "published" / f"{listener_id}.json"

    def version_path(self, field_version: str) -> Path:
        return self.root / "fields" / f"{field_version}.json"

    def read_published(self, listener_id: str) -> Optional[Dict[str, Any]]:
        path = self.published_path(listener_id)
        if not path.exists():
            return None
        try:
            return read_json(path)
        except (OSError, ValueError):
            return None

    def publish_field(self, listener_id: str, field_document: Dict[str, Any]) -> str:
        payload = dict(field_document)
        field_version = payload.pop("field_version", None)
        if not field_version:
            field_version = hash_obj(payload)
            payload["field_version"] = field_version
        else:
            payload["field_version"] = field_version
        version_path = self.version_path(field_version)
        if not version_path.exists():
            atomic_write_json(version_path, payload)
        atomic_write_json(self.published_path(listener_id), payload)
        return field_version

    def state_path(self, listener_id: str) -> Path:
        return self.root / "state" / f"{listener_id}.json"

    def read_state(self, listener_id: str) -> Optional[Dict[str, Any]]:
        path = self.state_path(listener_id)
        if not path.exists():
            return None
        try:
            return read_json(path)
        except (OSError, ValueError):
            return None

    def write_state(self, listener_id: str, state: Mapping[str, Any]) -> None:
        atomic_write_json(self.state_path(listener_id), dict(state))

