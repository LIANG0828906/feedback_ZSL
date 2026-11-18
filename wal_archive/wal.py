"""WAL filename parsing and content validation.

The sample WAL format used by the exercise is deliberately small (512 KiB)
but keeps PostgreSQL's 8 KiB page layout, long/short page headers, WAL magic
and timeline identifiers.  Each sample page also carries a CRC32 trailer,
identified by a private bit in ``xlp_info``.  The validator additionally
understands PostgreSQL-style record CRCs as a fallback.
"""

from __future__ import annotations

import hashlib
import os
import re
import struct
import zlib
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .errors import E_SEGMENT_CHANGED, err


WAL_MAGIC = 0xD118
WAL_BLOCK_SIZE = 8192
LONG_HEADER_INFO = 0x0002
PAGE_CHECKSUM_INFO = 0x4000

DEFAULT_SEGMENT_SIZE = 512 * 1024
SAMPLE_SYSID = 0x310A11CE5EED0001

_WAL_NAME_RE = re.compile(r"^[0-9A-Fa-f]{24}$")


def segments_per_log(segment_size: int) -> int:
    if segment_size <= 0 or (1 << 32) % segment_size != 0:
        raise ValueError("segment_size must be a positive divisor of 2^32")
    return (1 << 32) // segment_size


def wal_filename(timeline: int, segno: int, segment_size: int) -> str:
    if timeline <= 0:
        raise ValueError("timeline must be positive")
    if segno < 0:
        raise ValueError("segno must be non-negative")
    per_log = segments_per_log(segment_size)
    log = segno // per_log
    seg = segno % per_log
    if log > 0xFFFFFFFF:
        raise ValueError("segno is too large")
    return f"{timeline:08X}{log:08X}{seg:08X}"


def parse_wal_filename(name: str, segment_size: int) -> dict[str, Any]:
    """Parse a 24-hex-digit WAL segment name.

    Raises a stable ``WAL003`` error for every parse failure.  Lowercase
    input is accepted and normalised; the archive itself is expected to use
    PostgreSQL's uppercase convention.
    """

    if not _WAL_NAME_RE.match(name):
        code, errname = ("WAL003", "E_WAL_NAME_INVALID")
        raise err(
            code,
            errname,
            "WAL segment name must be exactly 24 hexadecimal characters",
            details={"filename": name},
        )
    upper = name.upper()
    timeline = int(upper[0:8], 16)
    log = int(upper[8:16], 16)
    seg = int(upper[16:24], 16)
    per_log = segments_per_log(segment_size)
    if timeline == 0:
        code, errname = ("WAL003", "E_WAL_NAME_INVALID")
        raise err(
            code,
            errname,
            "WAL segment timeline must be greater than zero",
            details={"filename": name},
        )
    if seg >= per_log:
        code, errname = ("WAL003", "E_WAL_NAME_INVALID")
        raise err(
            code,
            errname,
            "WAL segment number is outside the configured segment size",
            details={
                "filename": name,
                "segment": seg,
                "segments_per_log": per_log,
            },
        )
    segno = log * per_log + seg
    return {
        "timeline": timeline,
        "log": log,
        "segment": seg,
        "segno": segno,
        "filename": upper,
    }


def _make_crc32c_table() -> tuple[int, ...]:
    poly = 0x82F63B78
    table: list[int] = []
    for value in range(256):
        crc = value
        for _ in range(8):
            crc = (crc >> 1) ^ poly if crc & 1 else crc >> 1
        table.append(crc & 0xFFFFFFFF)
    return tuple(table)


_CRC32C_TABLE = _make_crc32c_table()


def crc32c(data: bytes | bytearray | memoryview, crc: int = 0) -> int:
    """Small, dependency-free CRC32C (Castagnoli) implementation."""

    value = crc ^ 0xFFFFFFFF
    table = _CRC32C_TABLE
    for byte in data:
        value = table[(value ^ byte) & 0xFF] ^ (value >> 8)
    return value ^ 0xFFFFFFFF


def _u16(data: bytes, offset: int) -> int:
    return struct.unpack_from("<H", data, offset)[0]


def _u32(data: bytes, offset: int) -> int:
    return struct.unpack_from("<I", data, offset)[0]


def _u64(data: bytes, offset: int) -> int:
    return struct.unpack_from("<Q", data, offset)[0]


@dataclass
class SegmentValidation:
    classification: str
    usable: bool
    digest: str
    size: int
    mtime_ns: int
    header_ok: bool
    length_ok: bool
    pages_total: int
    pages_parsed: int
    pages_checked: int
    pages_ok: int
    pages_bad: int
    pages_unchecked: int
    checksum_pass_rate: float
    reason: str
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        return {
            "classification": self.classification,
            "usable": self.usable,
            "digest": self.digest,
            "size": self.size,
            "header_ok": self.header_ok,
            "length_ok": self.length_ok,
            "pages_total": self.pages_total,
            "pages_parsed": self.pages_parsed,
            "pages_checked": self.pages_checked,
            "pages_ok": self.pages_ok,
            "pages_bad": self.pages_bad,
            "pages_unchecked": self.pages_unchecked,
            "checksum_pass_rate": self.checksum_pass_rate,
            "reason": self.reason,
        }


def _stat_key(stat: os.stat_result) -> tuple[int, int, int, int]:
    return (
        int(stat.st_size),
        int(getattr(stat, "st_mtime_ns", int(stat.st_mtime * 1_000_000_000))),
        int(getattr(stat, "st_ino", 0) or 0),
        int(getattr(stat, "st_dev", 0) or 0),
    )


def _read_exact(handle: Any, size: int) -> bytes:
    chunks: list[bytes] = []
    remaining = size
    while remaining > 0:
        chunk = handle.read(remaining)
        if not chunk:
            break
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def _record_checksum_status(
    page: bytes, data_offset: int, page_end: int
) -> bool | None:
    """Return True/False for a self-contained record CRC, or None if unknown."""

    available = page_end - data_offset
    if available < 24:
        return None
    total_len = _u32(page, data_offset)
    if total_len < 24 or total_len > available:
        return None
    record = bytearray(page[data_offset : data_offset + total_len])
    stored = _u32(record, 20)
    record[20:24] = b"\x00\x00\x00\x00"
    return crc32c(record) == stored


def validate_segment(
    path: Path,
    *,
    expected_size: int = DEFAULT_SEGMENT_SIZE,
    expected_timeline: int | None = None,
    expected_segno: int | None = None,
) -> SegmentValidation:
    """Validate a segment without modifying it.

    The file is read once.  A size/mtime/inode change during the read is
    reported as ``E_SEGMENT_CHANGED_DURING_SCAN`` and must be treated as an
    in-progress write by the caller.
    """

    before = os.stat(path)
    digest = hashlib.sha256()
    size = 0
    pages_total = (before.st_size + WAL_BLOCK_SIZE - 1) // WAL_BLOCK_SIZE
    full_pages = before.st_size // WAL_BLOCK_SIZE
    pages_parsed = 0
    pages_checked = 0
    pages_ok = 0
    pages_bad = 0
    pages_unchecked = 0
    header_ok = True
    length_ok = before.st_size == expected_size
    reasons: list[str] = []
    header_timeline: int | None = None
    header_seg_size: int | None = None
    header_block_size: int | None = None

    try:
        with open(path, "rb") as handle:
            for page_index in range(full_pages):
                page = _read_exact(handle, WAL_BLOCK_SIZE)
                if len(page) != WAL_BLOCK_SIZE:
                    raise OSError("short read while scanning WAL page")
                digest.update(page)
                size += len(page)

                if page.count(0) == len(page):
                    pages_unchecked += 1
                    continue

                magic = _u16(page, 0)
                info = _u16(page, 2)
                if magic != WAL_MAGIC:
                    header_ok = False
                    pages_bad += 1
                    reasons.append(f"page {page_index}: bad WAL magic")
                    continue
                pages_parsed += 1
                page_tli = _u32(page, 4)
                page_addr = _u64(page, 8)
                if page_index == 0:
                    header_timeline = page_tli
                    absolute_page0 = (
                        (expected_segno * expected_size) & 0xFFFFFFFFFFFFFFFF
                        if expected_segno is not None
                        else None
                    )
                    if page_addr not in (0, absolute_page0):
                        header_ok = False
                        reasons.append("page 0: page address mismatch")
                    if not (info & LONG_HEADER_INFO):
                        header_ok = False
                        reasons.append("page 0: long page header flag is missing")
                    if len(page) >= 36:
                        header_seg_size = _u32(page, 28)
                        header_block_size = _u32(page, 32)
                else:
                    relative_addr = page_index * WAL_BLOCK_SIZE
                    absolute_addr = None
                    if expected_segno is not None:
                        absolute_addr = (
                            expected_segno * expected_size + relative_addr
                        ) & 0xFFFFFFFFFFFFFFFF
                    if page_addr not in (
                        relative_addr,
                        absolute_addr,
                    ):
                        header_ok = False
                        reasons.append(f"page {page_index}: page address mismatch")

                if (
                    expected_timeline is not None
                    and page_tli != expected_timeline
                    and not (page_index > 0 and page_tli == 0)
                ):
                    header_ok = False
                    reasons.append(f"page {page_index}: timeline mismatch")

                if info & PAGE_CHECKSUM_INFO:
                    pages_checked += 1
                    stored = _u32(page, WAL_BLOCK_SIZE - 4)
                    computed = zlib.crc32(page[:-4]) & 0xFFFFFFFF
                    if stored == computed:
                        pages_ok += 1
                    else:
                        pages_bad += 1
                        reasons.append(f"page {page_index}: page checksum mismatch")
                    continue

                data_offset = 40 if page_index == 0 else 24
                page_end = WAL_BLOCK_SIZE
                if page_end - data_offset <= 0:
                    pages_unchecked += 1
                elif page.count(0, data_offset, page_end) == page_end - data_offset:
                    pages_unchecked += 1
                else:
                    status = _record_checksum_status(page, data_offset, page_end)
                    if status is True:
                        pages_checked += 1
                        pages_ok += 1
                    elif status is False:
                        pages_checked += 1
                        pages_bad += 1
                        reasons.append(f"page {page_index}: record checksum mismatch")
                    else:
                        pages_unchecked += 1

            remainder = before.st_size % WAL_BLOCK_SIZE
            if remainder:
                tail = _read_exact(handle, remainder)
                digest.update(tail)
                size += len(tail)
            fstat_after = os.fstat(handle.fileno())
    except OSError as exc:
        code, name = ("WAL004", "E_SEGMENT_READ_ERROR")
        detail = getattr(exc, "strerror", None) or exc.__class__.__name__
        raise err(
            code,
            name,
            f"unable to read WAL segment: {detail}",
        ) from exc

    try:
        stat_after = os.stat(path)
    except OSError as exc:
        code, name = E_SEGMENT_CHANGED
        raise err(
            code,
            name,
            "WAL segment disappeared while it was being validated",
        ) from exc

    if _stat_key(before) != _stat_key(fstat_after) or _stat_key(before) != _stat_key(
        stat_after
    ):
        code, name = E_SEGMENT_CHANGED
        raise err(
            code,
            name,
            "WAL segment changed while it was being validated",
        )

    if header_seg_size is not None and header_seg_size != expected_size:
        header_ok = False
        reasons.append(
            f"segment size header {header_seg_size} does not match {expected_size}"
        )
    if header_block_size is not None and header_block_size != WAL_BLOCK_SIZE:
        header_ok = False
        reasons.append(
            f"block size header {header_block_size} does not match {WAL_BLOCK_SIZE}"
        )

    checksum_pass_rate = (
        float(pages_ok) / float(pages_checked) if pages_checked else 0.0
    )

    if size != expected_size:
        if size < expected_size and (size == 0 or header_ok):
            classification = "truncated"
            reason = "segment is shorter than the configured segment size"
        else:
            classification = "checksum_failed"
            reason = "segment length does not match the configured segment size"
        if reasons:
            reason = f"{reason}; " + "; ".join(reasons[:4])
        return SegmentValidation(
            classification=classification,
            usable=False,
            digest=digest.hexdigest(),
            size=size,
            mtime_ns=int(getattr(before, "st_mtime_ns", 0)),
            header_ok=header_ok,
            length_ok=False,
            pages_total=pages_total,
            pages_parsed=pages_parsed,
            pages_checked=pages_checked,
            pages_ok=pages_ok,
            pages_bad=pages_bad,
            pages_unchecked=pages_unchecked,
            checksum_pass_rate=checksum_pass_rate,
            reason=reason,
            details={"violations": reasons[:16]},
        )

    if not header_ok or pages_bad:
        classification = "checksum_failed"
        reason = "segment header or page checksum verification failed"
        if reasons:
            reason = "; ".join(reasons[:4])
    elif pages_checked == 0:
        classification = "checksum_failed"
        reason = "no page checksum could be parsed"
    elif pages_ok != pages_checked:
        classification = "checksum_failed"
        reason = "one or more page checksums failed"
    else:
        classification = "complete"
        reason = "all page headers and checksums passed"

    return SegmentValidation(
        classification=classification,
        usable=classification == "complete",
        digest=digest.hexdigest(),
        size=size,
        mtime_ns=int(getattr(before, "st_mtime_ns", 0)),
        header_ok=header_ok,
        length_ok=True,
        pages_total=pages_total,
        pages_parsed=pages_parsed,
        pages_checked=pages_checked,
        pages_ok=pages_ok,
        pages_bad=pages_bad,
        pages_unchecked=pages_unchecked,
        checksum_pass_rate=checksum_pass_rate,
        reason=reason,
        details={"violations": reasons[:16]},
    )


def utc_from_ns(ns: int) -> str:
    seconds, nanos = divmod(ns, 1_000_000_000)
    dt = datetime.fromtimestamp(seconds, tz=timezone.utc).replace(microsecond=nanos // 1000)
    return dt.isoformat(timespec="microseconds").replace("+00:00", "Z")


def parse_utc(value: str) -> datetime:
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    dt = datetime.fromisoformat(text)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def canonical_utc(value: str) -> str:
    return parse_utc(value).isoformat(timespec="microseconds").replace("+00:00", "Z")
