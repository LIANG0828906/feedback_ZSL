from __future__ import annotations

from dataclasses import dataclass
from typing import Dict, Iterable, List, Mapping, Optional, Tuple

from .checkpoint import PackedShardState
from .errors import BusinessError, E_INTERNAL


@dataclass(frozen=True)
class RecoveryEntry:
    step: int
    shard_id: int
    payload: bytes
    consumed_samples: int


class RecoveryWAL:
    """In-memory authoritative shard journal used after the latest checkpoint."""

    def __init__(self) -> None:
        self._entries: Dict[Tuple[int, int], RecoveryEntry] = {}
        self._latest_step = -1

    def append(self, step: int, shards: Mapping[int, PackedShardState]) -> None:
        if step <= self._latest_step:
            raise BusinessError(
                E_INTERNAL,
                "recovery journal must be appended in strictly increasing step order",
                {"step": step, "latest_step": self._latest_step},
            )
        for shard_id, packed in shards.items():
            self._entries[(shard_id, step)] = RecoveryEntry(
                step=step,
                shard_id=shard_id,
                payload=packed.payload,
                consumed_samples=packed.consumed_samples,
            )
        self._latest_step = step

    @property
    def latest_step(self) -> int:
        return self._latest_step

    def recover_shard(
        self,
        shard_id: int,
        anchor_step: int,
        current_step: int,
        live_payload: bytes,
        anchor_payload: Optional[bytes] = None,
    ) -> RecoveryEntry:
        if current_step < anchor_step:
            raise BusinessError(
                E_INTERNAL,
                "recovery anchor is newer than the live training state",
                {"anchor_step": anchor_step, "current_step": current_step},
            )
        anchor_entry = self._entries.get((shard_id, anchor_step))
        if anchor_entry is None:
            raise BusinessError(
                E_INTERNAL,
                "recovery journal does not contain the checkpoint anchor",
                {"shard_id": shard_id, "anchor_step": anchor_step},
            )
        if anchor_payload is not None and anchor_entry.payload != anchor_payload:
            raise BusinessError(
                E_INTERNAL,
                "checkpoint state does not match the recovery journal anchor",
                {"shard_id": shard_id, "anchor_step": anchor_step},
            )
        if current_step == anchor_step:
            return RecoveryEntry(
                step=current_step,
                shard_id=shard_id,
                payload=live_payload,
                consumed_samples=-1,
            )
        last: Optional[RecoveryEntry] = None
        for step in range(anchor_step + 1, current_step + 1):
            entry = self._entries.get((shard_id, step))
            if entry is None:
                raise BusinessError(
                    E_INTERNAL,
                    "recovery journal has a gap",
                    {"shard_id": shard_id, "missing_step": step},
                )
            last = entry
        assert last is not None
        if last.payload != live_payload:
            raise BusinessError(
                E_INTERNAL,
                "recovery journal diverges from the live shard state",
                {"shard_id": shard_id, "step": current_step},
            )
        return last
