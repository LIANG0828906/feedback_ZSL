"""Checkpoint commit and recovery.

Layout inside the run directory:
  ckpt/header.json          mapping table, bind-alignment intermediates,
                            run config, chain genesis hash (committed first)
  ckpt/interval_NNNNNN.json per-interval poses, quant events, error and
                            scale records, content hash + hash-chain link
  ckpt/manifest.json        last committed interval + chain head

Every commit goes through a temp file + atomic rename. Recovery trusts
only on-disk snapshots: it re-verifies the snapshot version and the
hash chain interval by interval and reports the first broken frame as
E_RECOVERY_INVALID. Recovery itself never writes the exit report.
"""

import os

from . import SNAPSHOT_VERSION
from .errors import E_RECOVERY_INVALID, RetargetError
from .util import content_hash, hash_text, read_json, write_json

HEADER = "header.json"
MANIFEST = "manifest.json"


def _interval_name(index):
    return f"interval_{index:06d}.json"


class CheckpointManager:
    def __init__(self, run_dir):
        self.run_dir = run_dir
        self.ckpt_dir = os.path.join(run_dir, "ckpt")

    def init_run(self, header):
        os.makedirs(self.ckpt_dir, exist_ok=True)
        header = dict(header)
        header["snapshot_version"] = SNAPSHOT_VERSION
        genesis = hash_text(
            content_hash(header["run_config"])
            + header["mapping_hash"] + header["bind_hash"])
        header["chain_hash"] = genesis
        write_json(os.path.join(self.ckpt_dir, HEADER), header)
        self._write_manifest({
            "snapshot_version": SNAPSHOT_VERSION,
            "committed_intervals": 0,
            "total_frames": header["run_config"]["total_frames"],
            "interval_frames": header["run_config"]["interval_frames"],
            "head_hash": genesis,
            "complete": False,
        })
        return genesis

    def commit_interval(self, index, payload, prev_hash):
        body = {
            "snapshot_version": SNAPSHOT_VERSION,
            "interval": index,
            "frame_start": payload["frame_start"],
            "frame_end": payload["frame_end"],
            "poses": payload["poses"],
            "quant_events": payload["quant_events"],
            "error_records": payload["error_records"],
            "scale_records": payload["scale_records"],
        }
        body["content_hash"] = content_hash(body)
        body["prev_hash"] = prev_hash
        chain = hash_text(prev_hash + body["content_hash"])
        body["chain_hash"] = chain
        write_json(os.path.join(self.ckpt_dir, _interval_name(index)), body)
        manifest = read_json(os.path.join(self.ckpt_dir, MANIFEST))
        manifest["committed_intervals"] = index + 1
        manifest["head_hash"] = chain
        self._write_manifest(manifest)
        return chain

    def mark_complete(self):
        manifest = read_json(os.path.join(self.ckpt_dir, MANIFEST))
        manifest["complete"] = True
        self._write_manifest(manifest)

    def _write_manifest(self, manifest):
        write_json(os.path.join(self.ckpt_dir, MANIFEST), manifest)

    def has_run(self):
        return os.path.exists(os.path.join(self.ckpt_dir, MANIFEST))

    def load_verified(self):
        """Verify header + hash chain from disk only.

        Returns (header, manifest, intervals). Raises E_RECOVERY_INVALID
        locating the first broken frame.
        """
        try:
            header = read_json(os.path.join(self.ckpt_dir, HEADER))
            manifest = read_json(os.path.join(self.ckpt_dir, MANIFEST))
        except (OSError, ValueError) as exc:
            raise RetargetError(E_RECOVERY_INVALID,
                                f"checkpoint unreadable: {exc}",
                                {"first_broken_frame": 0})
        for doc, where in ((header, "header"), (manifest, "manifest")):
            if doc.get("snapshot_version") != SNAPSHOT_VERSION:
                raise RetargetError(
                    E_RECOVERY_INVALID,
                    f"snapshot version mismatch in {where}",
                    {"first_broken_frame": 0,
                     "found": doc.get("snapshot_version"),
                     "expected": SNAPSHOT_VERSION})
        genesis = hash_text(
            content_hash(header["run_config"])
            + header["mapping_hash"] + header["bind_hash"])
        if genesis != header["chain_hash"]:
            raise RetargetError(E_RECOVERY_INVALID,
                                "header hash chain broken",
                                {"first_broken_frame": 0})

        intervals = []
        prev = genesis
        for idx in range(manifest["committed_intervals"]):
            path = os.path.join(self.ckpt_dir, _interval_name(idx))
            try:
                doc = read_json(path)
            except (OSError, ValueError) as exc:
                raise RetargetError(
                    E_RECOVERY_INVALID,
                    f"interval {idx} unreadable: {exc}",
                    {"first_broken_frame": self._frame_of(idx, manifest)})
            if doc.get("snapshot_version") != SNAPSHOT_VERSION or \
                    doc.get("prev_hash") != prev:
                raise RetargetError(
                    E_RECOVERY_INVALID,
                    f"hash chain link broken at interval {idx}",
                    {"first_broken_frame": doc.get(
                        "frame_start", self._frame_of(idx, manifest))})
            body = {k: v for k, v in doc.items()
                    if k not in ("content_hash", "prev_hash", "chain_hash")}
            if content_hash(body) != doc["content_hash"] or \
                    hash_text(prev + doc["content_hash"]) != doc["chain_hash"]:
                raise RetargetError(
                    E_RECOVERY_INVALID,
                    f"content hash mismatch at interval {idx}",
                    {"first_broken_frame": doc["frame_start"]})
            prev = doc["chain_hash"]
            intervals.append(doc)
        if prev != manifest["head_hash"]:
            raise RetargetError(
                E_RECOVERY_INVALID,
                "manifest head hash does not match verified chain",
                {"first_broken_frame": self._frame_of(
                    max(0, manifest["committed_intervals"] - 1), manifest)})
        return header, manifest, intervals

    @staticmethod
    def _frame_of(index, manifest):
        return index * manifest["interval_frames"]
