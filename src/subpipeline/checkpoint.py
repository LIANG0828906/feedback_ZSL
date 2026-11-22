"""Atomic, serialised checkpoint storage and version-gated recovery."""

import json
import os
import tempfile
import uuid
from threading import RLock

from .errors import E_DECODE, PipelineError
from .model import Screen, SubtitleResult, TradeOffRecord
from .version import ALGORITHM_VERSION

META_NAME = "snapshot_meta.json"
RECORDS_DIR = "records"


class CheckpointStore:
    """One run directory per pipeline run; never shared across tasks."""

    def __init__(self, run_dir, project_hash, params_hash,
                 algorithm_version=ALGORITHM_VERSION):
        self.run_dir = run_dir
        self.project_hash = project_hash
        self.params_hash = params_hash
        self.algorithm_version = algorithm_version
        os.makedirs(os.path.join(run_dir, RECORDS_DIR), exist_ok=True)
        self._lock = RLock()

    def _meta_path(self):
        return os.path.join(self.run_dir, META_NAME)

    def write_meta(self):
        meta = {
            "project_hash": self.project_hash,
            "params_hash": self.params_hash,
            "algorithm_version": self.algorithm_version,
        }
        self._atomic_write(self._meta_path(), meta)

    def _atomic_write(self, path, payload):
        directory = os.path.dirname(path)
        fd, tmp = tempfile.mkstemp(prefix=".tmp_", dir=directory)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                json.dump(payload, fh, ensure_ascii=False, indent=2,
                          sort_keys=True)
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, path)
        except OSError:
            if os.path.exists(tmp):
                os.unlink(tmp)
            raise

    def append_commit(self, result, sequence):
        """Serialised atomic write of one committed subtitle."""
        record = {
            "sequence": sequence,
            "result": result.to_dict(),
            "project_hash": self.project_hash,
            "params_hash": self.params_hash,
            "algorithm_version": self.algorithm_version,
        }
        path = os.path.join(
            self.run_dir, RECORDS_DIR,
            f"sub_{result.subtitle_id:06d}.json")
        with self._lock:
            self._atomic_write(path, record)

    def load(self):
        """Return (results_in_commit_order, meta) or ([], None) when invalid.

        Any version/hash mismatch invalidates the whole snapshot: callers must
        re-layout every subtitle rather than mixing old and new results.
        """
        with self._lock:
            meta_path = self._meta_path()
            if not os.path.exists(meta_path):
                return [], None
            try:
                with open(meta_path, encoding="utf-8") as fh:
                    meta = json.load(fh)
            except (OSError, json.JSONDecodeError) as exc:
                raise PipelineError(E_DECODE, f"corrupt snapshot meta: {exc}")
            if meta.get("project_hash") != self.project_hash or \
                    meta.get("params_hash") != self.params_hash or \
                    meta.get("algorithm_version") != self.algorithm_version:
                return [], "invalidated"
            records_dir = os.path.join(self.run_dir, RECORDS_DIR)
            files = sorted(f for f in os.listdir(records_dir)
                           if f.endswith(".json"))
            results = []
            for name in files:
                try:
                    with open(os.path.join(records_dir, name),
                              encoding="utf-8") as fh:
                        record = json.load(fh)
                except (OSError, json.JSONDecodeError) as exc:
                    raise PipelineError(E_DECODE,
                                        f"corrupt checkpoint record {name}: {exc}")
                if record.get("project_hash") != self.project_hash or \
                        record.get("params_hash") != self.params_hash or \
                        record.get("algorithm_version") != self.algorithm_version:
                    return [], "invalidated"
                results.append((record["sequence"],
                                _result_from_dict(record["result"])))
            results.sort(key=lambda item: item[0])
            return [item[1] for item in results], meta

    def invalidate(self):
        """Discard the whole snapshot (version mismatch recovery)."""
        with self._lock:
            records_dir = os.path.join(self.run_dir, RECORDS_DIR)
            if os.path.isdir(records_dir):
                for name in os.listdir(records_dir):
                    os.unlink(os.path.join(records_dir, name))
            if os.path.exists(self._meta_path()):
                os.unlink(self._meta_path())


def _result_from_dict(data):
    screens = tuple(Screen(tuple(s["lines"])) for s in data["screens"])
    tradeoffs = tuple(TradeOffRecord(**t) for t in data["tradeoffs"])
    return SubtitleResult(
        subtitle_id=data["subtitle_id"],
        segment_ref=data["segment_ref"],
        start_sample=data["start_sample"],
        end_sample=data["end_sample"],
        screens=screens,
        required_duration_s=data["required_duration_s"],
        actual_duration_s=data["actual_duration_s"],
        tradeoffs=tradeoffs,
        read_speed_cps=data["read_speed_cps"],
    )


def new_run_dir(root, project_id, run_id=None):
    """Create a unique, task-specific run directory under root."""
    run_id = run_id or uuid.uuid4().hex[:12]
    run_dir = os.path.join(root, f"run_{project_id}_{run_id}")
    os.makedirs(os.path.join(run_dir, RECORDS_DIR), exist_ok=True)
    return run_dir
