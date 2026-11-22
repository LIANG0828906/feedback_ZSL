"""Persistent, atomic, serial per-block checkpoints.

Each committed block is appended as one checksummed JSON line through a single
shared lock, so writes from worker threads can never interleave.  A block file
is first written to a temp name and ``os.replace``-d into place (atomic).

On restart the whole snapshot is validated: if the image content hash, the
parameter-version signature, or the algorithm version of any persisted block
does not match the current run, the *entire* snapshot is discarded and a full
recompute is forced -- partial mixing of stale cached blocks is forbidden.
"""

import hashlib
import json
import os
import shutil
import threading

from .errors import ForensicsError, E_VER
from .params import ALGORITHM_VERSION


def _record_checksum(record):
    payload = json.dumps(record, sort_keys=True, separators=(",", ":")).encode(
        "utf-8"
    )
    return hashlib.sha256(payload).hexdigest()


class CheckpointStore:
    def __init__(self, root, image_name):
        self.dir = os.path.join(root, image_name)
        self._lock = threading.Lock()
        os.makedirs(self.dir, exist_ok=True)

    def _block_path(self, block_id):
        return os.path.join(self.dir, "block_%06d.json" % block_id)

    def _tmp_path(self, block_id):
        return os.path.join(self.dir, ".block_%06d.tmp.%d" % (block_id, os.getpid()))

    def _header_path(self):
        return os.path.join(self.dir, "snapshot.json")

    def write_block(self, record):
        """Serialised atomic write of one committed block."""
        record = dict(record)
        record["checksum"] = _record_checksum(
            {k: v for k, v in record.items() if k != "checksum"}
        )
        block_id = int(record["block_id"])
        target = self._block_path(block_id)
        tmp = self._tmp_path(block_id)
        with self._lock:
            with open(tmp, "w", encoding="utf-8") as fh:
                json.dump(record, fh, sort_keys=True)
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, target)

    def write_header(self, image_content_hash, version_signature, algorithm_version):
        header = {
            "image_content_hash": image_content_hash,
            "version_signature": version_signature,
            "algorithm_version": algorithm_version,
        }
        tmp = os.path.join(self.dir, ".snapshot.tmp.%d" % os.getpid())
        with self._lock:
            with open(tmp, "w", encoding="utf-8") as fh:
                json.dump(header, fh, sort_keys=True)
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, self._header_path())

    def _read_block(self, block_id):
        path = self._block_path(block_id)
        with open(path, "r", encoding="utf-8") as fh:
            record = json.load(fh)
        stored = record.pop("checksum", None)
        if stored != _record_checksum(record):
            raise ForensicsError(
                E_VER,
                "checksum mismatch for persisted block %d" % block_id,
                block_id=block_id,
            )
        return record

    def load(self, image_content_hash, version_signature, expected_block_count):
        """Validate and load the snapshot; discard everything on any mismatch."""
        header_path = self._header_path()
        if not os.path.exists(header_path):
            return {}
        with open(header_path, "r", encoding="utf-8") as fh:
            header = json.load(fh)

        mismatch = None
        if header.get("image_content_hash") != image_content_hash:
            mismatch = "image_content_hash"
        elif header.get("version_signature") != version_signature:
            mismatch = "version_signature"
        elif header.get("algorithm_version") != ALGORITHM_VERSION:
            mismatch = "algorithm_version"

        if mismatch is not None:
            # Whole snapshot is invalid: wipe and force full recompute.
            self.invalidate()
            raise ForensicsError(
                E_VER,
                "checkpoint %s mismatch; snapshot invalidated, full recompute required"
                % mismatch,
                field=mismatch,
            )

        records = {}
        for block_id in range(expected_block_count):
            path = self._block_path(block_id)
            if not os.path.exists(path):
                continue
            record = self._read_block(block_id)
            if (
                record.get("image_content_hash") != image_content_hash
                or record.get("version_signature") != version_signature
                or record.get("algorithm_version") != ALGORITHM_VERSION
            ):
                self.invalidate()
                raise ForensicsError(
                    E_VER,
                    "block %d version identity mismatch; snapshot invalidated"
                    % block_id,
                    block_id=block_id,
                )
            records[block_id] = record
        return records

    def invalidate(self):
        with self._lock:
            for name in os.listdir(self.dir):
                path = os.path.join(self.dir, name)
                if os.path.isfile(path):
                    os.remove(path)
                else:
                    shutil.rmtree(path)

    def persisted_ids(self):
        ids = []
        for name in os.listdir(self.dir):
            if name.startswith("block_") and name.endswith(".json"):
                ids.append(int(name[len("block_") : -len(".json")]))
        return sorted(ids)
