"""Lock-free immutable snapshot publication for concurrent query threads."""

from __future__ import annotations

import copy
from collections import OrderedDict
from dataclasses import dataclass
from typing import Any, Mapping

from .errors import ErrorCode, ToolError
from .hashing import sha256_json


@dataclass(frozen=True)
class Snapshot:
    step: int
    groups: Mapping[str, Mapping[str, Any]]
    last_loss: float
    cumulative_loss: float
    graph_version: str
    cache_hashes: Mapping[str, str]
    checkpoint_hash: str
    published: bool
    content_hash: str

    @property
    def handle(self) -> str:
        return f"{self.step}:{self.checkpoint_hash}"

    def to_ordered(self) -> OrderedDict[str, Any]:
        return OrderedDict(
            (
                ("handle", self.handle),
                ("step", self.step),
                ("groups", OrderedDict(
                    (group, OrderedDict(values)) for group, values in self.groups.items()
                )),
                ("last_loss", self.last_loss),
                ("cumulative_loss", self.cumulative_loss),
                ("graph_version", self.graph_version),
                ("cache_hashes", OrderedDict(self.cache_hashes)),
                ("checkpoint_hash", self.checkpoint_hash),
                ("content_hash", self.content_hash),
            )
        )


def make_snapshot(
    *,
    step: int,
    groups: Mapping[str, Mapping[str, Any]],
    last_loss: float,
    cumulative_loss: float,
    graph_version: str,
    cache_hashes: Mapping[str, str],
    checkpoint_hash: str,
    published: bool,
) -> Snapshot:
    ordered_groups = OrderedDict(
        (group_id, OrderedDict(values)) for group_id, values in groups.items()
    )
    ordered_cache = OrderedDict(cache_hashes)
    content_hash = sha256_json(
        OrderedDict(
            (
                ("step", int(step)),
                ("groups", ordered_groups),
                ("last_loss", float(last_loss)),
                ("cumulative_loss", float(cumulative_loss)),
                ("graph_version", graph_version),
                ("cache_hashes", ordered_cache),
                ("checkpoint_hash", checkpoint_hash),
            )
        )
    )
    return Snapshot(
        step=int(step),
        groups=ordered_groups,
        last_loss=float(last_loss),
        cumulative_loss=float(cumulative_loss),
        graph_version=graph_version,
        cache_hashes=ordered_cache,
        checkpoint_hash=checkpoint_hash,
        published=bool(published),
        content_hash=content_hash,
    )


class SnapshotPublisher:
    """A single reference swap publishes a fully-built immutable snapshot."""

    def __init__(self) -> None:
        self._current: Snapshot | None = None
        self._unpublished: dict[str, Snapshot] = {}

    @property
    def current(self) -> Snapshot | None:
        return self._current

    def prepare(self, **kwargs: Any) -> Snapshot:
        snapshot = make_snapshot(published=False, **kwargs)
        self._unpublished[snapshot.handle] = snapshot
        return snapshot

    def publish(self, snapshot: Snapshot | None = None, **kwargs: Any) -> Snapshot:
        if snapshot is None:
            snapshot = make_snapshot(published=True, **kwargs)
        elif not snapshot.published:
            snapshot = make_snapshot(
                step=snapshot.step,
                groups=snapshot.groups,
                last_loss=snapshot.last_loss,
                cumulative_loss=snapshot.cumulative_loss,
                graph_version=snapshot.graph_version,
                cache_hashes=snapshot.cache_hashes,
                checkpoint_hash=snapshot.checkpoint_hash,
                published=True,
            )
        self._unpublished.pop(snapshot.handle, None)
        self._current = snapshot
        return snapshot

    def query(self, handle: str | int | None = None) -> Snapshot:
        current = self._current
        if current is None:
            raise ToolError(
                ErrorCode.E_SNAPSHOT_STALE,
                "no checkpoint snapshot has been published",
                details={"handle": handle},
            )
        if not current.published:
            raise ToolError(
                ErrorCode.E_SNAPSHOT_STALE,
                "snapshot handle has not been published",
                details={"handle": current.handle},
            )
        if handle is not None:
            requested = str(handle)
            current_handle = current.handle
            if requested not in (current_handle, str(current.step)):
                known_unpublished = requested in self._unpublished
                raise ToolError(
                    ErrorCode.E_SNAPSHOT_STALE,
                    "requested snapshot is stale or unpublished",
                    details={
                        "handle": requested,
                        "current_handle": current_handle,
                        "unpublished": known_unpublished,
                    },
                )
        return copy.deepcopy(current)

