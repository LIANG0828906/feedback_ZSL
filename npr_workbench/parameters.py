"""Thread-safe versioned camera/parameter snapshots."""

from __future__ import annotations

import copy
import threading
from collections import OrderedDict

from .errors import VersionConflictError
from .model import Camera, RenderParams, Snapshot


class ParameterStore:
    """The preview path always reads ``latest``; exact rendering locks one snapshot."""

    def __init__(
        self,
        content_hash: str,
        camera: Camera,
        params: RenderParams,
        *,
        initial_version: int = 1,
        history_limit: int = 32,
    ) -> None:
        if initial_version < 1:
            raise ValueError("initial_version must be >= 1")
        self._content_hash = content_hash
        self._lock = threading.RLock()
        self._version = int(initial_version)
        self._camera = camera
        self._params = params
        self._history: OrderedDict[int, Snapshot] = OrderedDict()
        self._history_limit = history_limit
        self._history[self._version] = self._make_snapshot(self._version)

    def _make_snapshot(self, version: int) -> Snapshot:
        return Snapshot(
            version=version,
            content_hash=self._content_hash,
            camera=copy.deepcopy(self._camera),
            params=copy.deepcopy(self._params),
        )

    @property
    def version(self) -> int:
        with self._lock:
            return self._version

    def latest(self) -> Snapshot:
        with self._lock:
            return self._history[self._version]

    def update(
        self,
        *,
        camera: Camera | None = None,
        params: RenderParams | None = None,
    ) -> Snapshot:
        with self._lock:
            if camera is None and params is None:
                return self._history[self._version]
            next_camera = copy.deepcopy(camera) if camera is not None else self._camera
            next_params = copy.deepcopy(params) if params is not None else self._params
            next_params.validate()
            self._version += 1
            self._camera = next_camera
            self._params = next_params
            snapshot = self._make_snapshot(self._version)
            self._history[self._version] = snapshot
            while len(self._history) > self._history_limit:
                self._history.popitem(last=False)
            return snapshot

    def snapshot(self, version: int | None = None) -> Snapshot:
        with self._lock:
            requested = self._version if version is None else int(version)
            if requested not in self._history:
                raise VersionConflictError(
                    requested,
                    self._version,
                    f"snapshot version {requested} is no longer available",
                )
            return self._history[requested]

