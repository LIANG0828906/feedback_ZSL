"""Preview consistency: immutable version snapshots and stale handle checks."""

import threading

from .errors import PipelineError
from .render import canvas_hash, render_version, write_png


class VersionSnapshot:
    """Immutable handle to one committed version; never partially updated."""

    def __init__(self, manifest, layer_hashes, canvas=None):
        self.version = manifest["version"]
        self.manifest_hash = manifest["manifest_hash"]
        self.layers = tuple(sorted({e["layer"] for e in manifest["entries"]}))
        self.batches = tuple(
            sorted({(e["layer"], e["batch"]) for e in manifest["entries"]})
        )
        self.layer_hashes = tuple(sorted(layer_hashes.items()))
        self._canvas = canvas

    @property
    def canvas(self):
        return self._canvas


class PreviewService:
    """Publishes whole snapshots; readers always see one coherent version."""

    def __init__(self, store, width=640, height=480, margin=24):
        self.store = store
        self.width = width
        self.height = height
        self.margin = margin
        self._lock = threading.RLock()
        self._current = None
        self._retired = set()

    def publish(self, manifest):
        """Render into an independent buffer, then swap the snapshot atomically."""
        canvas = render_version(
            self.store, manifest, self.width, self.height, self.margin
        )
        layer_hashes = {}
        for layer in sorted({e["layer"] for e in manifest["entries"]}):
            entries = [e for e in manifest["entries"] if e["layer"] == layer]
            layer_hashes[layer] = _entry_set_hash(entries)
        snapshot = VersionSnapshot( manifest, layer_hashes, canvas)
        with self._lock:
            if self._current is not None:
                self._retired.add(self._current.version)
            previous = self._current
            self._current = snapshot
        return snapshot, previous

    def current(self):
        with self._lock:
            return self._current

    def read_snapshot(self, expected_version=None):
        with self._lock:
            snapshot = self._current
            if snapshot is None:
                return None
            if expected_version is not None and snapshot.version != expected_version:
                raise PipelineError.stale_version(
                    f"snapshot v{expected_version} retired; current is v{snapshot.version}"
                )
            if snapshot.version in self._retired:
                raise PipelineError.stale_version(
                    f"snapshot v{snapshot.version} has been retired"
                )
            return snapshot

    def verify_handle(self, snapshot):
        """A handle kept across a publish fails once its version is retired."""
        with self._lock:
            if snapshot is None:
                raise PipelineError.stale_version("null snapshot handle")
            if snapshot.version in self._retired or (
                self._current is not None and snapshot.version != self._current.version
            ):
                raise PipelineError.stale_version(
                    f"handle v{snapshot.version} retired; current is v{self._current.version}"
                )
        return snapshot

    def save_png(self, path, snapshot=None):
        target = snapshot or self.current()
        if target is None:
            raise PipelineError.stale_version("no committed version to render")
        self.verify_handle(target)
        write_png(path, target.canvas)
        return canvas_hash(target.canvas)


def _entry_set_hash(entries):
    from .canonical import canonical_hash

    return canonical_hash(
        sorted(
            (
                e["layer"],
                e["batch"],
                e["chunk"],
                e["content_hash"],
            )
            for e in entries
        )
    )
