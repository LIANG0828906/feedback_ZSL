"""Deterministic generation of the small WAL archive used for exercises.

The generated archive has two PostgreSQL-like instances sharing one root,
two timelines, duplicate segment names across instances, conflicting
versions, truncated/corrupt segments, timeline markers, an internal gap and
segments that are still being written.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import shutil
import struct
import sys
import time
import zlib
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from .wal import (
    LONG_HEADER_INFO,
    PAGE_CHECKSUM_INFO,
    SAMPLE_SYSID,
    WAL_BLOCK_SIZE,
    WAL_MAGIC,
    crc32c,
    wal_filename,
)


DEFAULT_SEGMENT_SIZE = 512 * 1024
INSTANCE_A = "pg-55432"
INSTANCE_B = "pg-55433"


@dataclass
class GeneratedArchive:
    root: Path
    files: int
    logical_segments: int
    bytes_written: int
    timeline1_segments: int
    timeline2_segments: int
    conflicts: list[int]
    gaps: list[int]
    truncated: list[dict[str, Any]]
    checksum_failed: list[dict[str, Any]]
    writing: list[str]
    seed: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "files": self.files,
            "logical_segments": self.logical_segments,
            "bytes_written": self.bytes_written,
            "timeline1_segments": self.timeline1_segments,
            "timeline2_segments": self.timeline2_segments,
            "conflicts": self.conflicts,
            "gaps": self.gaps,
            "truncated": self.truncated,
            "checksum_failed": self.checksum_failed,
            "writing": self.writing,
            "seed": self.seed,
        }


def _iso(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).isoformat(timespec="seconds").replace(
        "+00:00", "Z"
    )


def _set_mtime(path: Path, dt: datetime) -> int:
    ns = int(dt.timestamp() * 1_000_000_000)
    os.utime(path, ns=(ns, ns))
    return ns


def _put_record(page: bytearray, offset: int, seed_value: int) -> None:
    payload_len = 256
    total_len = 24 + payload_len
    xid = seed_value & 0xFFFFFFFF
    prev = (seed_value << 8) & 0xFFFFFFFFFFFFFFFF
    struct.pack_into("<IIQBBHI", page, offset, total_len, xid, prev, 0, 0, 0, 0)
    rng = random.Random(seed_value)
    payload = bytes(rng.getrandbits(8) for _ in range(payload_len))
    page[offset + 24 : offset + 24 + payload_len] = payload
    crc = crc32c(page[offset : offset + total_len])
    struct.pack_into("<I", page, offset + 20, crc)


def build_segment_bytes(
    timeline: int,
    segno: int,
    *,
    segment_size: int = DEFAULT_SEGMENT_SIZE,
    variant: int = 0,
    seed: int = 310,
) -> bytes:
    """Build one valid sample WAL segment entirely in memory."""

    if segment_size % WAL_BLOCK_SIZE != 0:
        raise ValueError("sample segment size must be a multiple of 8192")
    pages = segment_size // WAL_BLOCK_SIZE
    result = bytearray(segment_size)
    for page_index in range(pages):
        start = page_index * WAL_BLOCK_SIZE
        info = PAGE_CHECKSUM_INFO | (LONG_HEADER_INFO if page_index == 0 else 0)
        struct.pack_into(
            "<HHIQI",
            result,
            start,
            WAL_MAGIC,
            info,
            timeline,
            page_index * WAL_BLOCK_SIZE,
            0,
        )
        if page_index == 0:
            struct.pack_into(
                "<QII", result, start + 20, SAMPLE_SYSID, segment_size, WAL_BLOCK_SIZE
            )
        data_offset = start + (40 if page_index == 0 else 24)
        record_seed = (
            (timeline * 0x9E3779B1)
            ^ (segno * 0x85EBCA77)
            ^ (variant * 0xC2B2AE3D)
            ^ (page_index * 0x27D4EB2F)
            ^ (seed * 0x165667B1)
            ^ 0x5A17
        ) & 0xFFFFFFFF
        _put_record(result, data_offset, record_seed)
        trailer = zlib.crc32(result[start : start + WAL_BLOCK_SIZE - 4]) & 0xFFFFFFFF
        struct.pack_into("<I", result, start + WAL_BLOCK_SIZE - 4, trailer)
    return bytes(result)


def _corrupt_page(data: bytes, page_index: int) -> bytes:
    mutable = bytearray(data)
    offset = page_index * WAL_BLOCK_SIZE + 80
    mutable[offset] ^= 0x5A
    return bytes(mutable)


def _write_bytes(path: Path, data: bytes, mtime: datetime) -> int:
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "wb") as handle:
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    _set_mtime(path, mtime)
    return len(data)


def _write_json(path: Path, payload: dict[str, Any], mtime: datetime) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(payload, sort_keys=True, indent=2) + "\n"
    path.write_text(text, encoding="utf-8", newline="\n")
    _set_mtime(path, mtime)


def _choose_special(count: int, gaps: list[int], blocked: set[int]) -> int:
    candidates = [value for value in range(2, max(3, count)) if value not in gaps]
    for value in candidates:
        if value not in blocked:
            return value
    raise RuntimeError("archive profile is too small to place all sample cases")


def generate_archive(
    root: str | os.PathLike[str],
    *,
    seed: int = 310,
    segment_size: int = DEFAULT_SEGMENT_SIZE,
    profile: str = "full",
    force: bool = False,
) -> GeneratedArchive:
    """Generate the deterministic archive.

    ``profile=full`` produces about 250 segment files (about 130 MiB).
    ``profile=small`` produces the same semantic cases with a much smaller
    footprint and is used by the test suite.
    """

    root_path = Path(root).resolve()
    if root_path.exists() and any(root_path.iterdir()):
        if not force:
            raise FileExistsError(
                f"archive root is not empty: {root_path}; pass force=True to replace"
            )
        shutil.rmtree(root_path)
    root_path.mkdir(parents=True, exist_ok=True)

    if profile == "full":
        a_t1, b_t1, b_t2, a_t2 = 125, 118, 8, 3
    elif profile == "small":
        a_t1, b_t1, b_t2, a_t2 = 20, 18, 4, 2
    else:
        raise ValueError(f"unknown profile: {profile}")

    gap_start = max(3, int(b_t1 * 0.4))
    if gap_start + 1 > b_t1 - 4:
        gap_start = max(2, b_t1 - 6)
    gaps = [gap_start, gap_start + 1]

    conflict_positions = sorted(
        {
            max(2, int(b_t1 * 0.2)),
            max(2, int(b_t1 * 0.5)),
            max(2, int(b_t1 * 0.75)),
            max(2, int(b_t1 * 0.9)),
        }
    )
    conflict_positions = [value for value in conflict_positions if value not in gaps]

    blocked = set(gaps) | set(conflict_positions)
    trunc_a = _choose_special(b_t1, gaps, blocked)
    blocked.add(trunc_a)
    trunc_b = _choose_special(b_t1, gaps, blocked)
    blocked.add(trunc_b)
    corrupt_a = _choose_special(b_t1, gaps, blocked)
    blocked.add(corrupt_a)
    corrupt_b = _choose_special(b_t1, gaps, blocked)

    base1 = datetime(2026, 1, 1, tzinfo=timezone.utc)
    base2 = base1 + timedelta(seconds=(b_t1 + 2) * 2 + 60)

    instances = (INSTANCE_A, INSTANCE_B)
    for instance in instances:
        (root_path / "instances" / instance / "segments").mkdir(parents=True)

    t1_last = base1 + timedelta(seconds=a_t1 * 2 + 2)
    markers = {
        "00000001.json": {
            "authoritative_instance": INSTANCE_A,
            "parent_timeline": None,
            "reason": "initial timeline",
            "switch_lsn": "0/0",
            "switch_segment": 1,
            "switched_at": _iso(base1),
            "timeline": 1,
        },
        "00000002.json": {
            "authoritative_instance": INSTANCE_B,
            "parent_timeline": 1,
            "reason": "promotion after local recovery",
            "switch_lsn": f"0/{0x1000000:08X}",
            "switch_segment": 1,
            "switched_at": _iso(t1_last),
            "timeline": 2,
        },
    }
    for name, marker in markers.items():
        _write_json(root_path / "timeline-markers" / name, marker, base1)

    files = 0
    bytes_written = 0
    logical = set()
    truncated: list[dict[str, Any]] = []
    checksum_failed: list[dict[str, Any]] = []
    writing: list[str] = []
    conflict_records: list[int] = []

    def add_segment(
        instance: str,
        timeline: int,
        segno: int,
        *,
        mtime: datetime,
        variant: int = 0,
        truncate_bytes: int | None = None,
        corrupt: bool = False,
    ) -> Path:
        nonlocal files, bytes_written
        filename = wal_filename(timeline, segno, segment_size)
        data = build_segment_bytes(
            timeline,
            segno,
            segment_size=segment_size,
            variant=variant,
            seed=seed,
        )
        if corrupt:
            data = _corrupt_page(data, min(3, segment_size // WAL_BLOCK_SIZE - 1))
        path = root_path / "instances" / instance / "segments" / filename
        if truncate_bytes is not None:
            data = data[:truncate_bytes]
        bytes_written += _write_bytes(path, data, mtime)
        files += 1
        logical.add((timeline, segno))
        return path

    for segno in range(1, a_t1 + 1):
        if segno in gaps:
            continue
        mtime = base1 + timedelta(seconds=segno * 2)
        if segno == trunc_a:
            add_segment(
                INSTANCE_A,
                1,
                segno,
                mtime=mtime,
                truncate_bytes=segment_size // 2 + 123,
            )
            truncated.append(
                {
                    "instance": INSTANCE_A,
                    "timeline": 1,
                    "segno": segno,
                    "kind": "truncated",
                }
            )
        elif segno == corrupt_a:
            add_segment(INSTANCE_A, 1, segno, mtime=mtime, corrupt=True)
            checksum_failed.append(
                {
                    "instance": INSTANCE_A,
                    "timeline": 1,
                    "segno": segno,
                    "kind": "checksum_failed",
                }
            )
        else:
            add_segment(INSTANCE_A, 1, segno, mtime=mtime)

    for segno in range(1, b_t1 + 1):
        if segno in gaps:
            continue
        mtime = base1 + timedelta(seconds=segno * 2)
        conflict = segno in conflict_positions
        if conflict:
            conflict_records.append(segno)
            # Alternate which instance completed first.  The timeline marker
            # still names the authoritative source, so both facts are visible.
            if segno % 2:
                mtime -= timedelta(seconds=1)
            else:
                mtime += timedelta(seconds=1)
        if segno == trunc_b:
            add_segment(
                INSTANCE_B,
                1,
                segno,
                mtime=mtime,
                truncate_bytes=segment_size // 3,
            )
            truncated.append(
                {
                    "instance": INSTANCE_B,
                    "timeline": 1,
                    "segno": segno,
                    "kind": "truncated",
                }
            )
        elif segno == corrupt_b:
            add_segment(
                INSTANCE_B,
                1,
                segno,
                mtime=mtime,
                variant=2,
                corrupt=True,
            )
            checksum_failed.append(
                {
                    "instance": INSTANCE_B,
                    "timeline": 1,
                    "segno": segno,
                    "kind": "checksum_failed",
                }
            )
        else:
            add_segment(
                INSTANCE_B,
                1,
                segno,
                mtime=mtime,
                variant=1 if conflict else 0,
            )

    for segno in range(1, b_t2 + 1):
        mtime = base2 + timedelta(seconds=segno * 2)
        add_segment(INSTANCE_B, 2, segno, mtime=mtime)
    for segno in range(b_t2 + 1, b_t2 + a_t2 + 1):
        mtime = base2 + timedelta(seconds=segno * 2)
        add_segment(INSTANCE_A, 2, segno, mtime=mtime)

    write_time = base2 + timedelta(seconds=(b_t2 + a_t2 + 5) * 2)
    partial_a = wal_filename(1, a_t1 + 1, segment_size) + ".partial"
    partial_b = wal_filename(2, b_t2 + 1, segment_size) + ".partial"
    partial_a_path = (
        root_path / "instances" / INSTANCE_A / "segments" / partial_a
    )
    partial_b_path = (
        root_path / "instances" / INSTANCE_B / "segments" / partial_b
    )
    partial_data = build_segment_bytes(
        1, a_t1 + 1, segment_size=segment_size, variant=7, seed=seed
    )[: segment_size // 5]
    bytes_written += _write_bytes(partial_a_path, partial_data, write_time)
    partial_data_b = build_segment_bytes(
        2, b_t2 + 1, segment_size=segment_size, variant=8, seed=seed
    )[: segment_size // 6]
    bytes_written += _write_bytes(partial_b_path, partial_data_b, write_time)
    writing.extend([partial_a, partial_b])

    writer_name = wal_filename(1, a_t1 + 2, segment_size)
    writer_path = (
        root_path / "instances" / INSTANCE_A / "segments" / writer_name
    )
    writer_data = build_segment_bytes(
        1, a_t1 + 2, segment_size=segment_size, variant=9, seed=seed
    )
    bytes_written += _write_bytes(writer_path, writer_data, write_time)
    writer_sidecar = writer_path.with_name(writer_path.name + ".writer.json")
    _write_json(
        writer_sidecar,
        {
            "filename": writer_name,
            "format": "wal-archive-writer/v1",
            "heartbeat_at": _iso(write_time),
            "instance": INSTANCE_A,
            "started_at": _iso(write_time - timedelta(seconds=1)),
        },
        write_time,
    )
    writing.append(writer_name)

    malformed = (
        root_path
        / "instances"
        / INSTANCE_A
        / "segments"
        / "not-a-wal-segment.wal"
    )
    malformed.write_bytes(b"not a WAL segment\n")
    _set_mtime(malformed, base1)

    stray = root_path / "instances" / "stray.txt"
    stray.write_text("unexpected archive-root entry\n", encoding="utf-8")
    _set_mtime(stray, base1)

    # Keep the metadata directory's mtime deterministic as well.
    for directory in sorted(root_path.rglob("*"), reverse=True):
        if directory.is_dir():
            _set_mtime(directory, base1)

    return GeneratedArchive(
        root=root_path,
        files=files,
        logical_segments=len(logical),
        bytes_written=bytes_written,
        timeline1_segments=a_t1 + b_t1 - 2 * len(gaps),
        timeline2_segments=b_t2 + a_t2,
        conflicts=conflict_records,
        gaps=gaps,
        truncated=truncated,
        checksum_failed=checksum_failed,
        writing=writing,
        seed=seed,
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Generate a deterministic sample WAL archive")
    parser.add_argument("--root", required=True, help="archive root directory")
    parser.add_argument("--seed", type=int, default=310)
    parser.add_argument("--profile", choices=("full", "small"), default="full")
    parser.add_argument("--segment-size", type=int, default=DEFAULT_SEGMENT_SIZE)
    parser.add_argument("--force", action="store_true")
    args = parser.parse_args(argv)

    started = time.monotonic()
    generated = generate_archive(
        args.root,
        seed=args.seed,
        segment_size=args.segment_size,
        profile=args.profile,
        force=args.force,
    )
    payload = generated.as_dict()
    payload["root_label"] = generated.root.name
    payload["duration_ms"] = int((time.monotonic() - started) * 1000)
    print(json.dumps(payload, sort_keys=True, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
