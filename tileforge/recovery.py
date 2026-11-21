"""Disk-only per-tile commit journal with an unbroken hash chain."""

from __future__ import annotations

import base64
import hashlib
import json
import shutil
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .binary import atomic_write_bytes
from .errors import E_RECOVERY_INVALID, error


REQUIRED_STATE_FIELDS = {
    "schema",
    "run_key",
    "params_version",
    "config_hash",
    "input_hash",
    "sync_marker",
    "affected_tile_ids",
    "base_version_hash",
    "base_manifest_path",
    "chain_seed",
}

REQUIRED_COMMIT_FIELDS = {
    "schema",
    "seq",
    "tile_id",
    "submit_order",
    "params_version",
    "config_hash",
    "input_hash",
    "sync_marker",
    "profile",
    "usage_bytes",
    "payload_b64",
    "output_hash",
    "content_hash",
    "exposure",
    "max_code_delta",
    "tone_map_curve",
    "prev_chain",
    "commit_hash",
}


def _canonical(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False).encode("utf-8")


@dataclass(frozen=True)
class RecoveredCommit:
    seq: int
    tile_id: int
    payload: bytes
    output_hash: str
    content_hash: str
    exposure: float
    max_code_delta: int
    tone_map_curve: str
    profile: str
    usage_bytes: dict[str, Any]
    commit_hash: str


class RecoveryJournal:
    def __init__(
        self,
        root: str | Path,
        *,
        input_hash: str,
        config_hash: str,
        sync_marker: str,
        affected_tile_ids: list[int],
        base_version_hash: str | None,
        base_manifest_path: str | None,
        params_version: int,
    ) -> None:
        self.root = Path(root)
        self.state_path = self.root / "state.json"
        self.commits_dir = self.root / "commits"
        identity = {
            "input_hash": input_hash,
            "config_hash": config_hash,
            "sync_marker": sync_marker,
            "params_version": params_version,
            "base_version_hash": base_version_hash,
            "affected_tile_ids": affected_tile_ids,
        }
        self.run_key = hashlib.sha256(_canonical(identity)).hexdigest()
        self.input_hash = input_hash
        self.config_hash = config_hash
        self.sync_marker = sync_marker
        self.affected_tile_ids = list(affected_tile_ids)
        self.base_version_hash = base_version_hash
        self.base_manifest_path = base_manifest_path
        self.params_version = params_version
        self.chain_seed = hashlib.sha256(
            _canonical(identity) + (base_version_hash or "").encode("ascii")
        ).hexdigest()

    @property
    def state(self) -> dict[str, Any]:
        return {
            "schema": 1,
            "run_key": self.run_key,
            "params_version": self.params_version,
            "config_hash": self.config_hash,
            "input_hash": self.input_hash,
            "sync_marker": self.sync_marker,
            "affected_tile_ids": self.affected_tile_ids,
            "base_version_hash": self.base_version_hash,
            "base_manifest_path": self.base_manifest_path,
            "chain_seed": self.chain_seed,
        }

    def initialize(self, restart: bool = False) -> None:
        if restart and self.root.exists():
            shutil.rmtree(self.root)
        self.commits_dir.mkdir(parents=True, exist_ok=True)
        orphaned = sorted(self.commits_dir.glob("*.json"))
        if not self.state_path.exists() and orphaned:
            raise error(
                E_RECOVERY_INVALID,
                "recovery state is missing while block commits exist",
                field="state.json",
                commits=[path.name for path in orphaned],
            )
        if self.state_path.exists():
            existing = self._read_json(self.state_path, "state")
            missing = REQUIRED_STATE_FIELDS - existing.keys()
            if missing:
                raise error(
                    E_RECOVERY_INVALID,
                    "recovery state is missing fields",
                    field="state.json",
                    missing=sorted(missing),
                )
            if existing != self.state:
                raise error(
                    E_RECOVERY_INVALID,
                    "recovery state belongs to a different parameter submission",
                    field="state.json",
                    expected=self.run_key,
                    actual=existing.get("run_key"),
                )
            return
        atomic_write_bytes(
            self.state_path,
            json.dumps(self.state, sort_keys=True, indent=2, ensure_ascii=True).encode("utf-8") + b"\n",
        )

    def _read_json(self, path: Path, label: str) -> dict[str, Any]:
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError, UnicodeError) as exc:
            raise error(
                E_RECOVERY_INVALID,
                "cannot read recovery state",
                field=label,
                path=str(path),
                reason=str(exc),
            ) from exc
        if not isinstance(value, dict):
            raise error(E_RECOVERY_INVALID, "recovery record is not an object", field=label, path=str(path))
        return value

    def _commit_hash(self, record: dict[str, Any], payload: bytes) -> str:
        body = {key: value for key, value in record.items() if key != "commit_hash"}
        return hashlib.sha256(_canonical(body) + payload).hexdigest()

    def load_valid(self) -> list[RecoveredCommit]:
        if not self.state_path.exists():
            return []
        state = self._read_json(self.state_path, "state")
        if state != self.state:
            raise error(
                E_RECOVERY_INVALID,
                "recovery state does not match the requested run",
                field="state.json",
                expected_run_key=self.run_key,
                actual_run_key=state.get("run_key"),
            )
        files = sorted(self.commits_dir.glob("*.json"))
        if not files:
            return []
        records: list[tuple[Path, dict[str, Any], bytes]] = []
        for path in files:
            record = self._read_json(path, f"commit:{path.name}")
            missing = REQUIRED_COMMIT_FIELDS - record.keys()
            if missing:
                raise error(
                    E_RECOVERY_INVALID,
                    "recovery commit is missing fields",
                    field=f"commit:{path.name}",
                    missing=sorted(missing),
                )
            try:
                payload = base64.b64decode(str(record["payload_b64"]), validate=True)
            except (ValueError, TypeError) as exc:
                raise error(E_RECOVERY_INVALID, "commit payload is not valid base64", field="payload_b64", file=path.name) from exc
            records.append((path, record, payload))
        records.sort(key=lambda item: int(item[1]["seq"]))
        prev = self.chain_seed
        commits: list[RecoveredCommit] = []
        for expected_seq, (path, record, payload) in enumerate(records):
            seq = record.get("seq")
            if not isinstance(seq, int) or seq != expected_seq:
                raise error(
                    E_RECOVERY_INVALID,
                    "recovery commit sequence has a gap",
                    field="seq",
                    file=path.name,
                    actual=seq,
                    expected=expected_seq,
                )
            if path.name != f"{seq:08d}.json":
                raise error(
                    E_RECOVERY_INVALID,
                    "recovery commit filename is inconsistent",
                    field="filename",
                    file=path.name,
                    expected=f"{seq:08d}.json",
                )
            if record.get("prev_chain") != prev:
                raise error(
                    E_RECOVERY_INVALID,
                    "recovery hash chain is broken",
                    field="prev_chain",
                    file=path.name,
                    actual=record.get("prev_chain"),
                    expected=prev,
                )
            if record.get("commit_hash") != self._commit_hash(record, payload):
                raise error(E_RECOVERY_INVALID, "recovery commit hash mismatch", field="commit_hash", file=path.name)
            if hashlib.sha256(payload).hexdigest() != record.get("output_hash"):
                raise error(E_RECOVERY_INVALID, "recovery payload hash mismatch", field="output_hash", file=path.name)
            for field_name, expected in (
                ("params_version", self.params_version),
                ("config_hash", self.config_hash),
                ("input_hash", self.input_hash),
                ("sync_marker", self.sync_marker),
            ):
                if record.get(field_name) != expected:
                    raise error(
                        E_RECOVERY_INVALID,
                        "recovery commit parameter version mismatch",
                        field=field_name,
                        file=path.name,
                        actual=record.get(field_name),
                        expected=expected,
                    )
            if expected_seq >= len(self.affected_tile_ids) or record.get("tile_id") != self.affected_tile_ids[expected_seq]:
                raise error(
                    E_RECOVERY_INVALID,
                    "recovery commit tile order mismatch",
                    field="tile_id",
                    file=path.name,
                    actual=record.get("tile_id"),
                    expected=self.affected_tile_ids[expected_seq] if expected_seq < len(self.affected_tile_ids) else None,
                )
            prev = str(record["commit_hash"])
            commits.append(
                RecoveredCommit(
                    seq=seq,
                    tile_id=int(record["tile_id"]),
                    payload=payload,
                    output_hash=str(record["output_hash"]),
                    content_hash=str(record["content_hash"]),
                    exposure=float(record["exposure"]),
                    max_code_delta=int(record["max_code_delta"]),
                    tone_map_curve=str(record["tone_map_curve"]),
                    profile=str(record["profile"]),
                    usage_bytes=dict(record["usage_bytes"]),
                    commit_hash=str(record["commit_hash"]),
                )
            )
        if len(commits) > len(self.affected_tile_ids):
            raise error(E_RECOVERY_INVALID, "recovery contains more commits than affected tiles", field="commits")
        return commits

    def commit(
        self,
        *,
        seq: int,
        tile_id: int,
        submit_order: int,
        payload: bytes,
        content_hash: str,
        exposure: float,
        max_code_delta: int,
        tone_map_curve: str,
        profile: str,
        usage_bytes: dict[str, Any],
        previous_chain: str,
    ) -> str:
        record: dict[str, Any] = {
            "schema": 1,
            "seq": seq,
            "tile_id": tile_id,
            "submit_order": submit_order,
            "params_version": self.params_version,
            "config_hash": self.config_hash,
            "input_hash": self.input_hash,
            "sync_marker": self.sync_marker,
            "profile": profile,
            "usage_bytes": usage_bytes,
            "payload_b64": base64.b64encode(payload).decode("ascii"),
            "output_hash": hashlib.sha256(payload).hexdigest(),
            "content_hash": content_hash,
            "exposure": exposure,
            "max_code_delta": max_code_delta,
            "tone_map_curve": tone_map_curve,
            "prev_chain": previous_chain,
        }
        record["commit_hash"] = self._commit_hash(record, payload)
        path = self.commits_dir / f"{seq:08d}.json"
        atomic_write_bytes(path, json.dumps(record, sort_keys=True, indent=2, ensure_ascii=True).encode("utf-8") + b"\n")
        return str(record["commit_hash"])

    def discard(self) -> None:
        if self.root.exists():
            shutil.rmtree(self.root)
