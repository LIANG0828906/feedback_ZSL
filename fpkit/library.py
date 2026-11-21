"""Fingerprint library: atomic record commits and a hash-chained manifest."""

import hashlib
import json
import os
from pathlib import Path

from .errors import Busy, LibMismatch, RecoveryInvalid

GENESIS = hashlib.sha256(b"fpkit-genesis").hexdigest()
FORMAT = "fpkit-lib/1"


def _canonical_bytes(obj):
    return json.dumps(obj, sort_keys=True, separators=(",", ":")).encode()


def _chain(prev, sample_id, fp_hash, batch_seq, config_version):
    payload = f"{prev}|{sample_id}|{fp_hash}|{batch_seq}|{config_version}".encode()
    return hashlib.sha256(payload).hexdigest()


def _atomic_write(path, data):
    tmp = path.parent / "tmp" / (path.name + ".tmp")
    tmp.parent.mkdir(parents=True, exist_ok=True)
    with open(tmp, "wb") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, path)


class Library:
    # Layout: records/<sample_id>.fp.json + manifest.json, both committed
    # via temp file + atomic rename; active_batch is the batch lock file.

    def __init__(self, lib_dir):
        self.dir = Path(lib_dir)
        self.records_dir = self.dir / "records"
        self.manifest_path = self.dir / "manifest.json"
        self.lock_path = self.dir / "active_batch"
        self.records_dir.mkdir(parents=True, exist_ok=True)
        (self.dir / "tmp").mkdir(exist_ok=True)

    def load_manifest(self):
        if not self.manifest_path.exists():
            return {
                "format": FORMAT,
                "config_version": None,
                "time_quant": None,
                "batch_seq": 0,
                "entries": [],
                "version_hash": GENESIS,
            }
        with open(self.manifest_path, "rb") as fh:
            return json.loads(fh.read().decode())

    def _write_manifest(self, manifest):
        _atomic_write(self.manifest_path, _canonical_bytes(manifest))

    def acquire_batch(self, batch_id):
        if self.lock_path.exists():
            raise Busy(
                "batch already running in this library",
                active_batch=self.lock_path.read_text().strip(),
            )
        self.lock_path.write_text(str(batch_id))

    def release_batch(self):
        self.lock_path.unlink(missing_ok=True)

    def commit(self, sample_id, records, fp_hash, batch_seq, config_version, time_quant):
        """Atomically commit one sample record, then the manifest entry."""
        rec = {
            "sample_id": sample_id,
            "batch_seq": batch_seq,
            "config_version": config_version,
            "fp_hash": fp_hash,
            "records": records,
        }
        _atomic_write(self.records_dir / f"{sample_id}.fp.json", _canonical_bytes(rec))
        manifest = self.load_manifest()
        entry = {
            "sample_id": sample_id,
            "fp_hash": fp_hash,
            "batch_seq": batch_seq,
            "config_version": config_version,
        }
        entry["chain_hash"] = _chain(
            manifest["version_hash"], sample_id, fp_hash, batch_seq, config_version
        )
        manifest["entries"].append(entry)
        manifest["version_hash"] = entry["chain_hash"]
        manifest["batch_seq"] = batch_seq
        manifest["config_version"] = config_version
        manifest["time_quant"] = time_quant
        self._write_manifest(manifest)

    def committed_ids(self):
        return [e["sample_id"] for e in self.load_manifest()["entries"]]

    def _check(self, error_cls):
        """Verify chain + record hashes; raise error_cls on first problem."""
        manifest = self.load_manifest()
        prev = GENESIS
        problem = None
        for entry in manifest["entries"]:
            sid = entry["sample_id"]
            required = ("fp_hash", "batch_seq", "config_version", "chain_hash")
            missing = [f for f in required if f not in entry]
            if missing:
                problem = (sid, f"missing field {missing[0]}")
                break
            expect = _chain(prev, sid, entry["fp_hash"], entry["batch_seq"],
                            entry["config_version"])
            if expect != entry["chain_hash"]:
                problem = (sid, "chain hash mismatch")
                break
            rec_path = self.records_dir / f"{sid}.fp.json"
            if not rec_path.exists():
                problem = (sid, "record file missing")
                break
            rec = json.loads(rec_path.read_bytes().decode())
            if rec.get("fp_hash") != entry["fp_hash"]:
                problem = (sid, "record fp_hash mismatch")
                break
            prev = entry["chain_hash"]
        if problem is None and manifest["version_hash"] != prev:
            tail = manifest["entries"][-1]["sample_id"] if manifest["entries"] else None
            problem = (tail, "version_hash mismatch")
        if problem is not None:
            sid, why = problem
            raise error_cls(why, sample_id=sid)
        return manifest

    def verify(self):
        """Query-time verification: E_LIB_MISMATCH on inconsistency."""
        return self._check(LibMismatch)

    def recover(self):
        """Disk-state-only recovery after a forced kill.

        Validates the manifest hash chain (E_RECOVERY_INVALID locating the
        first broken sample), drops stale locks/temp files, and returns the
        set of already-committed sample ids so the caller only recomputes
        what is missing and never double-writes committed samples.
        """
        manifest = self._check(RecoveryInvalid)
        self.release_batch()
        tmp_dir = self.dir / "tmp"
        for stale in sorted(tmp_dir.glob("*.tmp")):
            stale.unlink()
        committed = {e["sample_id"] for e in manifest["entries"]}
        for rec_path in sorted(self.records_dir.glob("*.fp.json")):
            if rec_path.name[: -len(".fp.json")] not in committed:
                rec_path.unlink()
        return committed

    def iter_records(self):
        for entry in self.load_manifest()["entries"]:
            rec_path = self.records_dir / f"{entry['sample_id']}.fp.json"
            yield entry, json.loads(rec_path.read_bytes().decode())
