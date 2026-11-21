"""Chunk routing, atomic commits, version manifests and crash recovery."""

import glob
import os

from .canonical import canonical_bytes, canonical_hash, read_json, write_json_atomic
from .errors import PipelineError

CHUNK_SIZE = 10.0
BATCH_SIZE = 48
MANIFEST_NAME = "manifest.json"


def chunk_coord(x, y):
    return int(x // CHUNK_SIZE), int(y // CHUNK_SIZE)


def route_strokes(strokes):
    """Split a deterministic stroke list into batches and (chunk, batch) files."""
    batches = {}
    for stroke in strokes:
        batch_id = stroke["id"] // BATCH_SIZE
        chunk = chunk_coord(stroke["x"], stroke["y"])
        batches.setdefault(batch_id, {}).setdefault(chunk, []).append(stroke)
    return batches


def chunk_filename(chunk_x, chunk_y):
    return f"c{chunk_x}_{chunk_y}.json"


def batch_dir(root, version, layer, batch_id):
    return os.path.join(root, "chunks", f"v{version}", f"L{layer}", f"b{batch_id}")


def write_chunk_file(path, strokes, layer, batch_id, chunk, factor_version):
    payload = {
        "layer": layer,
        "batch": batch_id,
        "chunk": [chunk[0], chunk[1]],
        "factor_version": factor_version,
        "strokes": strokes,
    }
    body = canonical_bytes(payload)
    content_hash = canonical_hash(payload)
    tmp = f"{path}.tmp"
    try:
        with open(tmp, "wb") as fh:
            fh.write(body)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except OSError as exc:
        try:
            if os.path.exists(tmp):
                os.unlink(tmp)
        except OSError:
            pass
        raise PipelineError.commit_failed(
            f"layer={layer} batch={batch_id} chunk={chunk}: {exc}"
        ) from exc
    return content_hash


class Store:
    def __init__(self, root):
        self.root = os.fspath(root)
        os.makedirs(os.path.join(self.root, "chunks"), exist_ok=True)
        self.manifest_path = os.path.join(self.root, MANIFEST_NAME)

    # ------------------------------------------------------------- manifest
    def load_manifest(self):
        if not os.path.exists(self.manifest_path):
            return None
        return read_json(self.manifest_path)

    def write_manifest(self, manifest):
        write_json_atomic(self.manifest_path, manifest)

    # ------------------------------------------------------------- commits
    def commit_version(
        self,
        version,
        layer_outputs,
        factors,
        parent_manifest=None,
        crash_hook=None,
        cancel_check=None,
    ):
        """Write all chunk files first, then atomically swap the manifest.

        layer_outputs: {layer: {"strokes": [...], "culled": [...],
                                "factor_hash": str}}
        """
        entries = []
        written_paths = []
        try:
            for layer in sorted(layer_outputs):
                out = layer_outputs[layer]
                batches = route_strokes(out["strokes"])
                for batch_id in sorted(batches):
                    directory = batch_dir(self.root, version, layer, batch_id)
                    os.makedirs(directory, exist_ok=True)
                    if crash_hook is not None:
                        crash_hook(layer, batch_id)
                    if cancel_check is not None:
                        cancel_check(layer, batch_id)
                    for chunk in sorted(batches[batch_id]):
                        strokes = sorted(batches[batch_id][chunk], key=lambda s: s["id"])
                        path = os.path.join(directory, chunk_filename(*chunk))
                        content_hash = self.write_chunk_file(
                            path, strokes, layer, batch_id, chunk, out["factor_hash"]
                        )
                        written_paths.append(path)
                        entries.append(
                            {
                                "version": version,
                                "layer": layer,
                                "batch": batch_id,
                                "chunk": [chunk[0], chunk[1]],
                                "content_hash": content_hash,
                                "factor_version": out["factor_hash"],
                            }
                        )
        except PipelineError:
            self._rollback(version, written_paths)
            raise
        except OSError as exc:
            self._rollback(version, written_paths)
            raise PipelineError.commit_failed(str(exc)) from exc

        manifest = {
            "version": version,
            "parent_version": None if parent_manifest is None else parent_manifest["version"],
            "parent_hash": None if parent_manifest is None else parent_manifest["manifest_hash"],
            "factors": factors,
            "entries": sorted(
                entries, key=lambda e: (e["layer"], e["batch"], e["chunk"])
            ),
            "culled": {
                str(layer): list(layer_outputs[layer].get("culled", []))
                for layer in sorted(layer_outputs)
            },
        }
        manifest["manifest_hash"] = _manifest_hash(manifest)
        try:
            self.write_manifest(manifest)
        except OSError as exc:
            self._rollback(version, written_paths)
            raise PipelineError.commit_failed(str(exc)) from exc
        return manifest

    def _rollback(self, version, written_paths):
        for path in written_paths:
            try:
                if os.path.exists(path):
                    os.unlink(path)
            except OSError:
                pass
        base = os.path.join(self.root, "chunks", f"v{version}")
        if os.path.isdir(base):
            for dir_path, _, files in os.walk(base, topdown=False):
                for name in files:
                    if name.endswith(".tmp"):
                        try:
                            os.unlink(os.path.join(dir_path, name))
                        except OSError:
                            pass

    def write_chunk_file(self, path, strokes, layer, batch_id, chunk, factor_version):
        return write_chunk_file(path, strokes, layer, batch_id, chunk, factor_version)

    # ------------------------------------------------------------- recovery
    def recover(self):
        """Validate the hash chain and prune uncommitted/temp chunk files."""
        manifest = self.load_manifest()
        if manifest is None:
            return None
        self._validate_chain(manifest)
        committed_versions = self._chain_versions(manifest)
        self._prune_uncommitted(committed_versions)
        return manifest

    def _validate_chain(self, manifest):
        current = manifest
        while True:
            expected = _manifest_hash(current)
            if current.get("manifest_hash") != expected:
                raise PipelineError.recovery_invalid(
                    f"manifest hash mismatch at version {current.get('version')}",
                    current.get("entries", [{}])[0].get("layer") if current.get("entries") else None,
                )
            for entry in current["entries"]:
                layer = entry["layer"]
                path = os.path.join(
                    batch_dir(self.root, current["version"], layer, entry["batch"]),
                    chunk_filename(*entry["chunk"]),
                )
                if not os.path.exists(path):
                    raise PipelineError.recovery_invalid(
                        f"missing chunk v{current['version']} L{layer} "
                        f"b{entry['batch']} chunk={tuple(entry['chunk'])}",
                        layer,
                    )
                with open(path, "rb"):
                    actual = canonical_hash(read_json(path))
                if actual != entry["content_hash"]:
                    raise PipelineError.recovery_invalid(
                        f"content hash broken v{current['version']} L{layer} "
                        f"b{entry['batch']}",
                        layer,
                    )
            if current["parent_version"] is None:
                break
            parent_path = self._historical_manifest_path(current["parent_version"])
            if not os.path.exists(parent_path):
                raise PipelineError.recovery_invalid(
                    f"parent manifest v{current['parent_version']} missing"
                )
            parent = read_json(parent_path)
            if parent.get("manifest_hash") != current["parent_hash"]:
                raise PipelineError.recovery_invalid(
                    "parent hash link broken", current["entries"][0]["layer"]
                )
            current = parent

    def _chain_versions(self, manifest):
        versions = set()
        current = manifest
        while True:
            versions.add(current["version"])
            if current["parent_version"] is None:
                return versions
            current = read_json(self._historical_manifest_path(current["parent_version"]))

    def _historical_manifest_path(self, version):
        return os.path.join(self.root, "manifests", f"manifest.v{version}.json")

    def _prune_uncommitted(self, committed_versions):
        chunks_root = os.path.join(self.root, "chunks")
        for tmp in glob.glob(os.path.join(chunks_root, "**", "*.tmp"), recursive=True):
            try:
                os.unlink(tmp)
            except OSError:
                pass
        for version_dir in glob.glob(os.path.join(chunks_root, "v*")):
            try:
                version = int(os.path.basename(version_dir)[1:])
            except ValueError:
                continue
            if version not in committed_versions:
                _rmtree(version_dir)

    def snapshot_historical_manifest(self, manifest):
        directory = os.path.join(self.root, "manifests")
        os.makedirs(directory, exist_ok=True)
        write_json_atomic(
            os.path.join(directory, f"manifest.v{manifest['version']}.json"), manifest
        )

    def read_layer_strokes(self, version, layer):
        base = os.path.join(self.root, "chunks", f"v{version}", f"L{layer}")
        strokes = []
        if not os.path.isdir(base):
            return strokes
        for path in sorted(glob.glob(os.path.join(base, "**", "*.json"), recursive=True)):
            doc = read_json(path)
            strokes.extend(doc["strokes"])
        strokes.sort(key=lambda s: s["id"])
        return strokes

    def layer_batches(self, manifest, layer):
        return sorted(
            {e["batch"] for e in manifest["entries"] if e["layer"] == layer}
        )


def _manifest_hash(manifest):
    payload = {k: v for k, v in manifest.items() if k != "manifest_hash"}
    return canonical_hash(payload)


def _rmtree(path):
    import shutil

    try:
        shutil.rmtree(path)
    except OSError:
        pass
