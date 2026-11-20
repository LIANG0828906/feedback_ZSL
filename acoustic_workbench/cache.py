from __future__ import annotations

import json
import os
import shutil
import tempfile
import fcntl
import threading
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .errors import E_CACHE_CORRUPT, fail
from .hashing import canonical_json, sha256_bytes, sha256_file, sha256_json


CACHE_SCHEMA = "acoustic-node-cache/v1"


def _jsonable(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(key): _jsonable(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_jsonable(item) for item in value]
    if isinstance(value, np.ndarray):
        return value.tolist()
    if isinstance(value, np.generic):
        return value.item()
    return value


def _payload_hash(
    point: str,
    node: str,
    cache_key: str,
    data: dict[str, Any],
    artifacts: dict[str, Any],
) -> str:
    return sha256_json(
        {
            "schema": CACHE_SCHEMA,
            "point": point,
            "node": node,
            "cache_key": cache_key,
            "data": data,
            "artifacts": artifacts,
        }
    )


@dataclass
class NodeResult:
    point: str
    node: str
    cache_key: str
    output_hash: str
    data: dict[str, Any]
    artifacts: dict[str, Any]
    paths: dict[str, Path] = field(default_factory=dict)

    def artifact_path(self, name: str) -> Path:
        try:
            return self.paths[name]
        except KeyError:
            fail(
                E_CACHE_CORRUPT,
                f"node {self.node} is missing artifact {name}",
                point=self.point,
            )


class CacheStore:
    """Content-addressed node cache. No timestamp participates in lookup."""

    def __init__(self, workdir: str | Path):
        self.workdir = Path(workdir)
        self.root = self.workdir / ".acoustic_cache"
        self.nodes_root = self.root / "nodes"
        self.tmp_root = self.root / "tmp"
        self.state_root = self.root / "state"
        self.lock_root = self.root / "locks"
        self.render_root = self.root / "renders"
        for path in (
            self.nodes_root,
            self.tmp_root,
            self.state_root,
            self.lock_root,
            self.render_root,
        ):
            path.mkdir(parents=True, exist_ok=True)
        self._state_thread_lock = threading.Lock()

    def node_dir(self, point: str, node: str, cache_key: str) -> Path:
        return self.nodes_root / point / node / cache_key

    def write_node(
        self,
        point: str,
        node: str,
        cache_key: str,
        data: dict[str, Any],
        artifacts: dict[str, np.ndarray | bytes | str],
    ) -> NodeResult:
        final_dir = self.node_dir(point, node, cache_key)
        if final_dir.exists():
            loaded = self.load_node(point, node, cache_key)
            if loaded is not None:
                return loaded
            fail(
                E_CACHE_CORRUPT,
                "existing node cache entry cannot be validated",
                point=point,
                node=node,
                cache_key=cache_key,
            )
        final_dir.parent.mkdir(parents=True, exist_ok=True)
        stage = Path(tempfile.mkdtemp(prefix="stage-", dir=self.tmp_root))
        artifact_manifest: dict[str, Any] = {}
        paths: dict[str, Path] = {}
        try:
            for name in sorted(artifacts):
                value = artifacts[name]
                destination = stage / name
                destination.parent.mkdir(parents=True, exist_ok=True)
                if isinstance(value, np.ndarray):
                    np.save(destination, np.ascontiguousarray(value), allow_pickle=False)
                    artifact_manifest[name] = {
                        "sha256": sha256_file(destination),
                        "size": destination.stat().st_size,
                        "kind": "npy",
                        "dtype": value.dtype.str,
                        "shape": list(value.shape),
                    }
                else:
                    if isinstance(value, str):
                        payload = value.encode("utf-8")
                    elif isinstance(value, bytes):
                        payload = value
                    else:
                        payload = canonical_json(_jsonable(value)).encode("utf-8")
                    destination.write_bytes(payload)
                    artifact_manifest[name] = {
                        "sha256": sha256_bytes(payload),
                        "size": len(payload),
                        "kind": "text" if isinstance(value, str) else "binary",
                    }
                paths[name] = final_dir / name
            json_data = _jsonable(data)
            output_hash = _payload_hash(
                point, node, cache_key, json_data, artifact_manifest
            )
            record = {
                "schema": CACHE_SCHEMA,
                "point": point,
                "node": node,
                "cache_key": cache_key,
                "output_hash": output_hash,
                "data": json_data,
                "artifacts": artifact_manifest,
            }
            (stage / "result.json").write_text(
                canonical_json(record),
                encoding="utf-8",
            )
            os.replace(stage, final_dir)
            return NodeResult(
                point=point,
                node=node,
                cache_key=cache_key,
                output_hash=output_hash,
                data=json_data,
                artifacts=artifact_manifest,
                paths=paths,
            )
        finally:
            if stage.exists():
                shutil.rmtree(stage, ignore_errors=True)

    def load_node(
        self,
        point: str,
        node: str,
        cache_key: str,
    ) -> NodeResult | None:
        directory = self.node_dir(point, node, cache_key)
        manifest_path = directory / "result.json"
        if not manifest_path.is_file():
            return None
        try:
            record = json.loads(manifest_path.read_text(encoding="utf-8"))
            if record.get("schema") != CACHE_SCHEMA:
                raise ValueError("unexpected cache schema")
            if (
                record.get("point") != point
                or record.get("node") != node
                or record.get("cache_key") != cache_key
            ):
                raise ValueError("cache identity mismatch")
            artifacts = record.get("artifacts", {})
            computed = _payload_hash(
                point,
                node,
                cache_key,
                record.get("data", {}),
                artifacts,
            )
            if computed != record.get("output_hash"):
                raise ValueError("cache payload hash mismatch")
            paths: dict[str, Path] = {}
            for name, artifact in artifacts.items():
                path = directory / name
                if not path.is_file():
                    raise ValueError(f"missing artifact {name}")
                if path.stat().st_size != int(artifact.get("size", -1)):
                    raise ValueError(f"artifact size mismatch {name}")
                if sha256_file(path) != artifact.get("sha256"):
                    raise ValueError(f"artifact content hash mismatch {name}")
                paths[name] = path
            return NodeResult(
                point=point,
                node=node,
                cache_key=cache_key,
                output_hash=str(record["output_hash"]),
                data=record.get("data", {}),
                artifacts=artifacts,
                paths=paths,
            )
        except (OSError, ValueError, TypeError, KeyError, json.JSONDecodeError):
            return None

    def write_heads(self, heads: dict[str, Any]) -> None:
        self._write_state("heads.json", heads)

    def update_head(self, point_id: str, entry: dict[str, Any]) -> None:
        with self._state_lock():
            heads = self.read_heads()
            heads[point_id] = entry
            self._write_state("heads.json", heads)

    def read_heads(self) -> dict[str, Any]:
        return self._read_state("heads.json")

    def write_render_heads(self, heads: dict[str, Any]) -> None:
        self._write_state("render_heads.json", heads)

    def update_render_head(self, point_id: str, entry: dict[str, Any]) -> None:
        with self._state_lock():
            heads = self.read_render_heads()
            heads[point_id] = entry
            self._write_state("render_heads.json", heads)

    def read_render_heads(self) -> dict[str, Any]:
        return self._read_state("render_heads.json")

    def _write_state(self, name: str, payload: dict[str, Any]) -> None:
        destination = self.state_root / name
        temporary = tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=self.state_root,
            prefix=f".{name}-",
            suffix=".tmp",
            delete=False,
        )
        temporary_path = Path(temporary.name)
        try:
            with temporary:
                temporary.write(canonical_json(_jsonable(payload)))
                temporary.flush()
                os.fsync(temporary.fileno())
            os.replace(temporary_path, destination)
        finally:
            if temporary_path.exists():
                temporary_path.unlink(missing_ok=True)

    @contextmanager
    def _state_lock(self):
        with self._state_thread_lock:
            path = self.lock_root / "state.lock"
            handle = path.open("a+")
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
                yield
            finally:
                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
                handle.close()

    def _read_state(self, name: str) -> dict[str, Any]:
        path = self.state_root / name
        if not path.is_file():
            return {}
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return {}
        return payload if isinstance(payload, dict) else {}

    def render_dir(self, point: str, cache_key: str) -> Path:
        return self.render_root / point / cache_key

    def write_render(
        self,
        point: str,
        cache_key: str,
        wav_bytes: bytes,
        metadata: dict[str, Any],
    ) -> dict[str, Any]:
        final_dir = self.render_dir(point, cache_key)
        final_dir.parent.mkdir(parents=True, exist_ok=True)
        if final_dir.is_dir():
            existing = self.load_render(point, cache_key)
            if existing is not None:
                return existing
        stage = Path(tempfile.mkdtemp(prefix="render-", dir=self.tmp_root))
        try:
            wav_path = stage / "audition.wav"
            wav_path.write_bytes(wav_bytes)
            record = {
                **metadata,
                "cache_key": cache_key,
                "wav": "audition.wav",
                "wav_sha256": sha256_bytes(wav_bytes),
                "wav_size": len(wav_bytes),
            }
            (stage / "render.json").write_text(
                canonical_json(_jsonable(record)),
                encoding="utf-8",
            )
            os.replace(stage, final_dir)
            return record
        finally:
            if stage.exists():
                shutil.rmtree(stage, ignore_errors=True)

    def load_render(
        self,
        point: str,
        cache_key: str,
    ) -> dict[str, Any] | None:
        directory = self.render_dir(point, cache_key)
        manifest = directory / "render.json"
        wav = directory / "audition.wav"
        if not manifest.is_file() or not wav.is_file():
            return None
        try:
            record = json.loads(manifest.read_text(encoding="utf-8"))
            if record.get("point_id") != point:
                return None
            if record.get("cache_key") != cache_key:
                return None
            if sha256_file(wav) != record.get("wav_sha256"):
                return None
            if wav.stat().st_size != record.get("wav_size"):
                return None
        except (OSError, json.JSONDecodeError):
            return None
        return record

    def render_path(self, point: str, cache_key: str) -> Path:
        return self.render_dir(point, cache_key) / "audition.wav"

