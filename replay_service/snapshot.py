"""Atomic snapshot commit and validated recovery."""

from __future__ import annotations

import json
import os

from .errors import RecoveryInvalidError, SnapshotWriteError
from .model import MODEL_VERSION, RoomState, chain_link, genesis_chain, \
    state_hash

REQUIRED_FIELDS = ("version", "room", "step", "state", "watermark",
                   "arrival_idx", "chain")


class SnapshotStore:
    def __init__(self, room_dir: str):
        self.room_dir = room_dir
        self.path = os.path.join(room_dir, "snapshot.json")
        self.tmp_path = os.path.join(room_dir, "snapshot.json.tmp")

    def commit(self, doc: dict) -> None:
        """Write tmp file + fsync + atomic rename.

        A failure never clobbers the last valid snapshot: the tmp file is
        simply left behind (and removed on a best-effort basis).
        """
        try:
            with open(self.tmp_path, "w", encoding="utf-8") as fh:
                json.dump(doc, fh, sort_keys=True, separators=(",", ":"))
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(self.tmp_path, self.path)
        except OSError as exc:
            try:
                if os.path.exists(self.tmp_path):
                    os.unlink(self.tmp_path)
            except OSError:
                pass
            raise SnapshotWriteError(
                "snapshot commit failed",
                step=doc.get("step"), error_class=type(exc).__name__,
                errno=getattr(exc, "errno", None)) from exc

    def exists(self) -> bool:
        return os.path.exists(self.path)

    def load(self) -> dict:
        """Load and fully validate a snapshot; any defect -> E_RECOVERY_INVALID."""
        try:
            with open(self.path, "r", encoding="utf-8") as fh:
                doc = json.load(fh)
        except FileNotFoundError as exc:
            raise RecoveryInvalidError("no snapshot on disk",
                                       reason="missing") from exc
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise RecoveryInvalidError(
                "snapshot unreadable (truncated or corrupt)",
                reason="truncated", detail=str(exc)) from exc
        except OSError as exc:
            raise RecoveryInvalidError("snapshot unreadable",
                                       reason=type(exc).__name__) from exc

        if not isinstance(doc, dict):
            raise RecoveryInvalidError("snapshot is not an object",
                                       reason="malformed")
        if doc.get("version") != MODEL_VERSION:
            raise RecoveryInvalidError(
                "snapshot model version mismatch", reason="version",
                found=doc.get("version"), expected=MODEL_VERSION)
        for field_name in REQUIRED_FIELDS:
            if field_name not in doc:
                raise RecoveryInvalidError("snapshot field missing",
                                           reason="missing_field",
                                           field=field_name)

        chain = doc["chain"]
        if not isinstance(chain, list):
            raise RecoveryInvalidError("hash chain is not a list",
                                       reason="malformed")
        prev = genesis_chain()
        for index, entry in enumerate(chain):
            expected_step = index + 1
            if entry.get("step") != expected_step:
                raise RecoveryInvalidError(
                    "hash chain step gap", reason="broken_chain",
                    first_broken_step=entry.get("step"),
                    expected_step=expected_step)
            if chain_link(prev, entry.get("state_hash", "")) != \
                    entry.get("chain_hash"):
                raise RecoveryInvalidError(
                    "hash chain link mismatch", reason="broken_chain",
                    first_broken_step=entry["step"])
            prev = entry["chain_hash"]

        state = RoomState.from_dict(doc["state"])
        current_hash = state_hash(state)
        if chain and chain[-1]["state_hash"] != current_hash:
            raise RecoveryInvalidError(
                "snapshot state does not match hash chain tip",
                reason="broken_chain", first_broken_step=chain[-1]["step"])
        if doc["step"] != len(chain):
            raise RecoveryInvalidError(
                "snapshot step does not match chain length",
                reason="broken_chain", first_broken_step=doc["step"])
        return doc
