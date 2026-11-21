"""JSON report persistence with atomic rename and hash-chain validation."""

from __future__ import annotations

import hashlib
import json
import os

from . import REPORT_VERSION
from .errors import E_REPORT_MISMATCH, SyncError


def _canonical(obj):
    return json.dumps(obj, sort_keys=True, separators=(",", ":"))


def _record_hash(record, prev_hash):
    return hashlib.sha256((prev_hash + _canonical(record)).encode()).hexdigest()


def build_report(dataset, config, results):
    """Assemble a report dict with a hash chain over per-channel records."""
    records = []
    prev = hashlib.sha256(b"syncbench-genesis").hexdigest()
    for record in results:
        entry = {"data": record, "prev_hash": prev}
        entry["hash"] = _record_hash(record, prev)
        records.append(entry)
        prev = entry["hash"]
    return {
        "version": REPORT_VERSION,
        "dataset": dataset,
        "config": config,
        "records": records,
        "chain_head": prev,
    }


def save_report(report, path):
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=2, sort_keys=True)
    os.replace(tmp, path)


def validate_report(report):
    if report.get("version") != REPORT_VERSION:
        raise SyncError(
            E_REPORT_MISMATCH,
            f"report version {report.get('version')} != {REPORT_VERSION}",
        )
    prev = hashlib.sha256(b"syncbench-genesis").hexdigest()
    for entry in report.get("records", []):
        if entry.get("prev_hash") != prev:
            raise SyncError(E_REPORT_MISMATCH, "hash chain link mismatch")
        if entry.get("hash") != _record_hash(entry["data"], prev):
            raise SyncError(E_REPORT_MISMATCH, "record hash mismatch")
        prev = entry["hash"]
    if report.get("chain_head") != prev:
        raise SyncError(E_REPORT_MISMATCH, "chain head mismatch")
    return True


def load_report(path):
    with open(path, "r", encoding="utf-8") as fh:
        report = json.load(fh)
    validate_report(report)
    return report
