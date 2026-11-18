"""Archive layout scanning and unified-view construction."""

from __future__ import annotations

import hashlib
import json
import os
import re
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

from . import errors
from .errors import ArchiveError
from .wal import (
    DEFAULT_SEGMENT_SIZE,
    SegmentValidation,
    parse_wal_filename,
    parse_utc,
    utc_from_ns,
    validate_segment,
)


INSTANCE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
TIMELINE_STEM_RE = re.compile(r"^[0-9A-Fa-f]{8}$")
WRITING_SUFFIXES = (".partial", ".tmp", ".inprogress", ".writing")
WRITER_SUFFIX = ".writer.json"


@dataclass
class ScanConfig:
    segment_size: int = DEFAULT_SEGMENT_SIZE
    writer_grace_seconds: float = 5.0
    stability_probe_seconds: float = 0.2

    def as_dict(self) -> dict[str, Any]:
        return {
            "segment_size": self.segment_size,
            "stability_probe_seconds": self.stability_probe_seconds,
            "writer_grace_seconds": self.writer_grace_seconds,
        }


@dataclass
class ScanError:
    code: str
    name: str
    message: str
    rel_path: str
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "name": self.name,
            "message": self.message,
            "path": self.rel_path,
        }
        if self.details:
            payload["details"] = self.details
        return payload


@dataclass
class ScanVersion:
    instance: str
    timeline: int
    segno: int
    filename: str
    rel_path: str
    digest: str
    size: int
    mtime_ns: int
    archived_at: str
    classification: str
    usable: bool
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
    conflict: bool = False

    @classmethod
    def from_validation(
        cls,
        *,
        instance: str,
        timeline: int,
        segno: int,
        filename: str,
        rel_path: str,
        validation: SegmentValidation,
    ) -> "ScanVersion":
        return cls(
            instance=instance,
            timeline=timeline,
            segno=segno,
            filename=filename,
            rel_path=rel_path,
            digest=validation.digest,
            size=validation.size,
            mtime_ns=validation.mtime_ns,
            archived_at=utc_from_ns(validation.mtime_ns),
            classification=validation.classification,
            usable=validation.usable,
            header_ok=validation.header_ok,
            length_ok=validation.length_ok,
            pages_total=validation.pages_total,
            pages_parsed=validation.pages_parsed,
            pages_checked=validation.pages_checked,
            pages_ok=validation.pages_ok,
            pages_bad=validation.pages_bad,
            pages_unchecked=validation.pages_unchecked,
            checksum_pass_rate=validation.checksum_pass_rate,
            reason=validation.reason,
            details=validation.details,
        )


@dataclass
class InProgressSegment:
    instance: str
    filename: str
    rel_path: str
    reason: str
    size: int
    mtime_ns: int
    timeline: int | None = None
    segno: int | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "filename": self.filename,
            "instance": self.instance,
            "mtime_ns": self.mtime_ns,
            "reason": self.reason,
            "rel_path": self.rel_path,
            "size": self.size,
            "state": "writing",
        }
        if self.timeline is not None:
            payload["timeline"] = self.timeline
        if self.segno is not None:
            payload["segno"] = self.segno
        if self.details:
            payload["details"] = self.details
        return payload


@dataclass
class TimelineMarker:
    timeline: int
    parent_timeline: int | None
    switch_segment: int | None
    switch_lsn: str | None
    switched_at: str
    authoritative_instance: str | None
    source_instance: str | None
    reason: str | None
    rel_path: str
    digest: str
    raw: dict[str, Any] = field(default_factory=dict)

    def as_dict(self) -> dict[str, Any]:
        return {
            "authoritative_instance": self.authoritative_instance,
            "digest": self.digest,
            "parent_timeline": self.parent_timeline,
            "reason": self.reason,
            "rel_path": self.rel_path,
            "source_instance": self.source_instance,
            "switch_lsn": self.switch_lsn,
            "switch_segment": self.switch_segment,
            "switched_at": self.switched_at,
            "timeline": self.timeline,
        }


@dataclass
class Quarantine:
    instance: str
    filename: str
    rel_path: str
    digest: str
    classification: str


@dataclass
class ScanSnapshot:
    root_label: str
    config: ScanConfig
    files_seen: int
    versions: list[ScanVersion]
    in_progress: list[InProgressSegment]
    errors: list[ScanError]
    markers: list[TimelineMarker]
    quarantines: list[Quarantine]
    archive_id: str

    @property
    def instances(self) -> list[str]:
        return sorted({version.instance for version in self.versions} | {
            item.instance for item in self.in_progress
        })


def _rel(root: Path, path: Path) -> str:
    try:
        return path.relative_to(root).as_posix()
    except ValueError:
        return path.name


def _error_to_scan(exc: ArchiveError) -> ScanError:
    return ScanError(
        code=exc.code,
        name=exc.name,
        message=exc.message,
        rel_path=exc.rel_path or "",
        details=exc.details,
    )


def _is_safe_regular_file(path: Path) -> bool:
    try:
        return path.is_file() and not path.is_symlink()
    except OSError:
        return False


def _stat(path: Path) -> os.stat_result:
    return path.lstat()


def _mtime_ns(stat: os.stat_result) -> int:
    return int(getattr(stat, "st_mtime_ns", int(stat.st_mtime * 1_000_000_000)))


def _stable_after_probe(path: Path, interval: float) -> bool:
    try:
        before = _stat(path)
        time.sleep(interval)
        after = _stat(path)
    except OSError:
        return False
    return (
        before.st_size == after.st_size
        and _mtime_ns(before) == _mtime_ns(after)
        and int(getattr(before, "st_ino", 0) or 0)
        == int(getattr(after, "st_ino", 0) or 0)
    )


def _base_filename(name: str) -> tuple[str, str | None]:
    for suffix in WRITING_SUFFIXES:
        if name.endswith(suffix):
            return name[: -len(suffix)], suffix
    return name, None


def _parse_ts(value: Any, fallback_ns: int) -> str:
    if isinstance(value, (int, float)):
        try:
            dt = datetime.fromtimestamp(float(value), tz=timezone.utc)
            return dt.isoformat(timespec="microseconds").replace("+00:00", "Z")
        except (OverflowError, OSError, ValueError):
            pass
    if isinstance(value, str):
        try:
            return (
                parse_utc(value)
                .isoformat(timespec="microseconds")
                .replace("+00:00", "Z")
            )
        except (ValueError, TypeError):
            pass
    return utc_from_ns(fallback_ns)


def _parse_marker(
    path: Path,
    rel_path: str,
    *,
    source_instance: str | None,
    segment_size: int,
) -> TimelineMarker:
    try:
        raw_bytes = path.read_bytes()
        stat = _stat(path)
    except OSError as exc:
        detail = getattr(exc, "strerror", None) or exc.__class__.__name__
        raise errors.marker_invalid(
            rel_path, f"unable to read marker: {detail}"
        ) from exc

    digest = hashlib.sha256(raw_bytes).hexdigest()
    stem = path.name.split(".", 1)[0]
    if not TIMELINE_STEM_RE.match(stem):
        raise errors.marker_invalid(
            rel_path, "timeline marker filename must be an 8-digit hexadecimal timeline"
        )

    if path.suffix.lower() == ".history":
        try:
            first = raw_bytes.decode("utf-8").splitlines()[0].split("\t")[0]
            parent = int(first, 16)
        except (IndexError, ValueError, UnicodeDecodeError) as exc:
            raise errors.marker_invalid(rel_path, "invalid PostgreSQL history file") from exc
        timeline = int(stem, 16)
        return TimelineMarker(
            timeline=timeline,
            parent_timeline=parent,
            switch_segment=None,
            switch_lsn=None,
            switched_at=utc_from_ns(_mtime_ns(stat)),
            authoritative_instance=None,
            source_instance=source_instance,
            reason="PostgreSQL timeline history",
            rel_path=rel_path,
            digest=digest,
            raw={"history": raw_bytes.decode("utf-8", "replace")},
        )

    try:
        payload = json.loads(raw_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise errors.marker_invalid(rel_path, f"marker is not valid JSON: {exc}") from exc
    if not isinstance(payload, dict):
        raise errors.marker_invalid(rel_path, "timeline marker must be a JSON object")

    timeline = payload.get("timeline")
    if not isinstance(timeline, int):
        try:
            timeline = int(stem, 16)
        except ValueError as exc:
            raise errors.marker_invalid(rel_path, "timeline is missing") from exc
    if timeline != int(stem, 16):
        raise errors.marker_invalid(
            rel_path, "marker timeline does not match its filename"
        )
    if timeline <= 0:
        raise errors.marker_invalid(rel_path, "timeline must be positive")

    parent = payload.get("parent_timeline")
    if parent is not None and not isinstance(parent, int):
        raise errors.marker_invalid(rel_path, "parent_timeline must be an integer or null")

    switch_segment = payload.get(
        "switch_segment",
        payload.get("switch_segno", payload.get("switch_point")),
    )
    switch_segno: int | None = None
    if isinstance(switch_segment, int):
        switch_segno = switch_segment
    elif isinstance(switch_segment, str):
        if re.match(r"^[0-9A-Fa-f]{24}$", switch_segment):
            try:
                parsed_switch = parse_wal_filename(switch_segment, segment_size)
            except ArchiveError as exc:
                raise errors.marker_invalid(
                    rel_path, f"invalid switch_segment WAL filename: {exc.message}"
                ) from exc
            if parsed_switch["timeline"] != timeline:
                raise errors.marker_invalid(
                    rel_path,
                    "switch_segment timeline does not match the marker timeline",
                )
            switch_segno = parsed_switch["segno"]
        else:
            try:
                switch_segno = int(switch_segment, 10)
            except ValueError as exc:
                raise errors.marker_invalid(
                    rel_path, "switch_segment must be a segment number or WAL filename"
                ) from exc
    elif switch_segment is not None:
        raise errors.marker_invalid(
            rel_path, "switch_segment must be a segment number or WAL filename"
        )

    authoritative = payload.get(
        "authoritative_instance",
        payload.get("authoritative_source", payload.get("owner_instance")),
    )
    if authoritative is not None and not isinstance(authoritative, str):
        raise errors.marker_invalid(
            rel_path, "authoritative_instance must be a string or null"
        )

    reason = payload.get("reason")
    if reason is not None and not isinstance(reason, str):
        reason = str(reason)

    return TimelineMarker(
        timeline=timeline,
        parent_timeline=parent,
        switch_segment=switch_segno,
        switch_lsn=payload.get("switch_lsn"),
        switched_at=_parse_ts(
            payload.get("switched_at", payload.get("switch_time")),
            _mtime_ns(stat),
        ),
        authoritative_instance=authoritative,
        source_instance=source_instance,
        reason=reason,
        rel_path=rel_path,
        digest=digest,
        raw=payload,
    )


def _scan_marker_dir(
    root: Path,
    directory: Path,
    *,
    source_instance: str | None,
    segment_size: int,
) -> tuple[list[TimelineMarker], list[ScanError]]:
    markers: list[TimelineMarker] = []
    errors_out: list[ScanError] = []
    if not directory.exists():
        return markers, errors_out
    if not directory.is_dir() or directory.is_symlink():
        errors_out.append(
            _error_to_scan(
                errors.layout_unexpected(
                    _rel(root, directory), "timeline marker path is not a directory"
                )
            )
        )
        return markers, errors_out
    for entry in sorted(directory.iterdir(), key=lambda item: item.name):
        rel_path = _rel(root, entry)
        if not _is_safe_regular_file(entry):
            errors_out.append(
                _error_to_scan(
                    errors.layout_unexpected(
                        rel_path, "timeline marker entry is not a regular file"
                    )
                )
            )
            continue
        if entry.suffix.lower() not in (".json", ".history"):
            errors_out.append(
                _error_to_scan(
                    errors.layout_unexpected(
                        rel_path, "unsupported timeline marker file extension"
                    )
                )
            )
            continue
        try:
            markers.append(
                _parse_marker(
                    entry,
                    rel_path,
                    source_instance=source_instance,
                    segment_size=segment_size,
                )
            )
        except ArchiveError as exc:
            if exc.rel_path is None:
                exc.rel_path = rel_path
            errors_out.append(_error_to_scan(exc))
    return markers, errors_out


def _scan_segment_dir(
    root: Path,
    instance: str,
    directory: Path,
    config: ScanConfig,
    quarantine_map: dict[tuple[str, str], Quarantine],
) -> tuple[
    list[ScanVersion],
    list[InProgressSegment],
    list[ScanError],
    list[Quarantine],
    int,
]:
    versions: list[ScanVersion] = []
    writing: list[InProgressSegment] = []
    errors_out: list[ScanError] = []
    new_quarantines: list[Quarantine] = []
    files_seen = 0
    now = time.time()

    if not directory.exists() or not directory.is_dir() or directory.is_symlink():
        errors_out.append(
            _error_to_scan(
                errors.layout_unexpected(
                    _rel(root, directory), "segments path is not a directory"
                )
            )
        )
        return versions, writing, errors_out, new_quarantines, files_seen

    entries = sorted(directory.iterdir(), key=lambda item: item.name)
    writer_bases = {
        entry.name[: -len(WRITER_SUFFIX)]
        for entry in entries
        if entry.name.endswith(WRITER_SUFFIX)
    }

    for entry in entries:
        rel_path = _rel(root, entry)
        name = entry.name
        if name.endswith(WRITER_SUFFIX):
            base = name[: -len(WRITER_SUFFIX)]
            base_path = entry.with_name(base)
            if not base_path.exists():
                try:
                    stat = _stat(entry)
                    size = int(stat.st_size)
                    mtime_ns = _mtime_ns(stat)
                except OSError:
                    size, mtime_ns = 0, 0
                timeline = segno = None
                try:
                    parsed = parse_wal_filename(base, config.segment_size)
                    timeline, segno = parsed["timeline"], parsed["segno"]
                except ArchiveError:
                    pass
                writing.append(
                    InProgressSegment(
                        instance=instance,
                        filename=base,
                        rel_path=rel_path,
                        reason="writer_marker_without_segment",
                        size=size,
                        mtime_ns=mtime_ns,
                        timeline=timeline,
                        segno=segno,
                    )
                )
            files_seen += 1
            continue

        if not _is_safe_regular_file(entry):
            errors_out.append(
                _error_to_scan(
                    errors.layout_unexpected(
                        rel_path, "segment entry is not a regular file"
                    )
                )
            )
            continue

        base, suffix = _base_filename(name)
        try:
            parsed = parse_wal_filename(base, config.segment_size)
        except ArchiveError as exc:
            exc.rel_path = rel_path
            errors_out.append(_error_to_scan(exc))
            continue

        if suffix is not None:
            files_seen += 1
            try:
                stat = _stat(entry)
                size = int(stat.st_size)
                mtime_ns = _mtime_ns(stat)
            except OSError:
                size, mtime_ns = 0, 0
            writing.append(
                InProgressSegment(
                    instance=instance,
                    filename=base,
                    rel_path=rel_path,
                    reason=f"staging_suffix:{suffix}",
                    size=size,
                    mtime_ns=mtime_ns,
                    timeline=parsed["timeline"],
                    segno=parsed["segno"],
                    details={"suffix": suffix},
                )
            )
            continue

        files_seen += 1
        if base in writer_bases:
            try:
                stat = _stat(entry)
                size = int(stat.st_size)
                mtime_ns = _mtime_ns(stat)
            except OSError:
                size, mtime_ns = 0, 0
            writing.append(
                InProgressSegment(
                    instance=instance,
                    filename=base,
                    rel_path=rel_path,
                    reason="writer_marker_present",
                    size=size,
                    mtime_ns=mtime_ns,
                    timeline=parsed["timeline"],
                    segno=parsed["segno"],
                )
            )
            continue

        try:
            stat_before = _stat(entry)
        except OSError as exc:
            detail = getattr(exc, "strerror", None) or exc.__class__.__name__
            errors_out.append(
                _error_to_scan(
                    errors.err(
                        "WAL004",
                        "E_SEGMENT_READ_ERROR",
                        f"unable to stat WAL segment: {detail}",
                        rel_path=rel_path,
                    )
                )
            )
            continue

        size_before = int(stat_before.st_size)
        mtime_before = _mtime_ns(stat_before)
        if (
            size_before < config.segment_size
            and now - (mtime_before / 1_000_000_000) <= config.writer_grace_seconds
            and not _stable_after_probe(entry, config.stability_probe_seconds)
        ):
            writing.append(
                InProgressSegment(
                    instance=instance,
                    filename=base,
                    rel_path=rel_path,
                    reason="size_or_mtime_changed_during_probe",
                    size=size_before,
                    mtime_ns=mtime_before,
                    timeline=parsed["timeline"],
                    segno=parsed["segno"],
                )
            )
            continue

        try:
            validation = validate_segment(
                entry,
                expected_size=config.segment_size,
                expected_timeline=parsed["timeline"],
                expected_segno=parsed["segno"],
            )
        except ArchiveError as exc:
            if exc.name == "E_SEGMENT_CHANGED_DURING_SCAN":
                try:
                    stat = _stat(entry)
                    size, mtime_ns = int(stat.st_size), _mtime_ns(stat)
                except OSError:
                    size, mtime_ns = 0, 0
                writing.append(
                    InProgressSegment(
                        instance=instance,
                        filename=base,
                        rel_path=rel_path,
                        reason="changed_during_validation",
                        size=size,
                        mtime_ns=mtime_ns,
                        timeline=parsed["timeline"],
                        segno=parsed["segno"],
                    )
                )
            else:
                exc.rel_path = rel_path
                errors_out.append(_error_to_scan(exc))
            continue

        classification = validation.classification
        reason = validation.reason
        usable = validation.usable
        if classification == "complete":
            existing = quarantine_map.get((instance, base))
            if existing is not None:
                classification = "truncated"
                reason = (
                    "sticky classification: this path was previously observed "
                    "as truncated and cannot become complete"
                )
                usable = False
        version = ScanVersion.from_validation(
            instance=instance,
            timeline=parsed["timeline"],
            segno=parsed["segno"],
            filename=base,
            rel_path=rel_path,
            validation=validation,
        )
        version.classification = classification
        version.reason = reason
        version.usable = usable
        versions.append(version)
        if classification == "truncated" and (instance, base) not in quarantine_map:
            q = Quarantine(
                instance=instance,
                filename=base,
                rel_path=rel_path,
                digest=version.digest,
                classification=classification,
            )
            new_quarantines.append(q)
            quarantine_map[(instance, base)] = q

    return versions, writing, errors_out, new_quarantines, files_seen


def _mark_conflicts(versions: Iterable[ScanVersion]) -> None:
    groups: dict[tuple[int, int], list[ScanVersion]] = {}
    for version in versions:
        groups.setdefault((version.timeline, version.segno), []).append(version)
    for group in groups.values():
        complete = [item for item in group if item.classification == "complete"]
        if not complete:
            continue
        ordered = sorted(
            complete,
            key=lambda item: (item.archived_at, item.instance, item.digest),
        )
        first_digest = ordered[0].digest
        for item in complete:
            item.conflict = item.digest != first_digest


def _archive_id(versions: Iterable[ScanVersion], markers: Iterable[TimelineMarker]) -> str:
    payload = {
        "markers": sorted(
            (
                {
                    "authoritative_instance": marker.authoritative_instance,
                    "digest": marker.digest,
                    "parent_timeline": marker.parent_timeline,
                    "switch_segment": marker.switch_segment,
                    "switched_at": marker.switched_at,
                    "timeline": marker.timeline,
                }
                for marker in markers
            ),
            key=lambda item: (item["timeline"], item["digest"]),
        ),
        "versions": sorted(
            (
                {
                    "classification": version.classification,
                    "digest": version.digest,
                    "filename": version.filename,
                    "instance": version.instance,
                    "segno": version.segno,
                    "size": version.size,
                    "timeline": version.timeline,
                }
                for version in versions
            ),
            key=lambda item: (
                item["instance"],
                item["timeline"],
                item["segno"],
                item["digest"],
            ),
        ),
    }
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(encoded).hexdigest()


def scan_archive(
    root: str | os.PathLike[str],
    *,
    config: ScanConfig | None = None,
    quarantines: Iterable[Quarantine] = (),
) -> ScanSnapshot:
    config = config or ScanConfig()
    root_path = Path(root).resolve()
    if not root_path.exists() or not root_path.is_dir():
        raise errors.root_missing(root_path.name)

    quarantine_map = {(item.instance, item.filename): item for item in quarantines}
    versions: list[ScanVersion] = []
    writing: list[InProgressSegment] = []
    scan_errors: list[ScanError] = []
    new_quarantines: list[Quarantine] = []
    markers: list[TimelineMarker] = []
    files_seen = 0
    saw_instances = False

    for entry in sorted(root_path.iterdir(), key=lambda item: item.name):
        rel_path = _rel(root_path, entry)
        if entry.name == "instances":
            saw_instances = True
            if not entry.is_dir() or entry.is_symlink():
                scan_errors.append(
                    _error_to_scan(
                        errors.layout_unexpected(
                            rel_path, "instances path is not a directory"
                        )
                    )
                )
                continue
            for instance_entry in sorted(entry.iterdir(), key=lambda item: item.name):
                instance_rel = _rel(root_path, instance_entry)
                if (
                    not instance_entry.is_dir()
                    or instance_entry.is_symlink()
                    or not INSTANCE_RE.match(instance_entry.name)
                ):
                    scan_errors.append(
                        _error_to_scan(
                            errors.layout_unexpected(
                                instance_rel, "invalid instance directory"
                            )
                        )
                    )
                    continue
                instance = instance_entry.name
                allowed = {"segments", "timeline-markers"}
                for child in sorted(
                    instance_entry.iterdir(), key=lambda item: item.name
                ):
                    child_rel = _rel(root_path, child)
                    if child.name not in allowed:
                        scan_errors.append(
                            _error_to_scan(
                                errors.layout_unexpected(
                                    child_rel,
                                    "unexpected entry in instance directory",
                                )
                            )
                        )
                        continue
                    if child.name == "segments":
                        (
                            found,
                            writing_found,
                            errors_found,
                            quarantines_found,
                            seen,
                        ) = _scan_segment_dir(
                            root_path,
                            instance,
                            child,
                            config,
                            quarantine_map,
                        )
                        versions.extend(found)
                        writing.extend(writing_found)
                        scan_errors.extend(errors_found)
                        new_quarantines.extend(quarantines_found)
                        files_seen += seen
                    else:
                        found, errors_found = _scan_marker_dir(
                            root_path,
                            child,
                            source_instance=instance,
                            segment_size=config.segment_size,
                        )
                        markers.extend(found)
                        scan_errors.extend(errors_found)
                        files_seen += len(found) + len(errors_found)
        elif entry.name == "timeline-markers":
            found, errors_found = _scan_marker_dir(
                root_path,
                entry,
                source_instance=None,
                segment_size=config.segment_size,
            )
            markers.extend(found)
            scan_errors.extend(errors_found)
            files_seen += len(found) + len(errors_found)
        else:
            scan_errors.append(
                _error_to_scan(
                    errors.layout_unexpected(
                        rel_path, "unexpected entry at archive root"
                    )
                )
            )

    if not saw_instances:
        scan_errors.append(
            _error_to_scan(
                errors.layout_unexpected(
                    "instances",
                    "required instances directory is missing at the archive root",
                )
            )
        )

    _mark_conflicts(versions)
    versions.sort(
        key=lambda item: (
            item.instance,
            item.timeline,
            item.segno,
            item.digest,
        )
    )
    writing.sort(key=lambda item: (item.instance, item.rel_path))
    scan_errors.sort(key=lambda item: (item.code, item.rel_path, item.message))
    markers.sort(key=lambda item: (item.timeline, item.digest))

    return ScanSnapshot(
        root_label=root_path.name,
        config=config,
        files_seen=files_seen,
        versions=versions,
        in_progress=writing,
        errors=scan_errors,
        markers=markers,
        quarantines=new_quarantines,
        archive_id=_archive_id(versions, markers),
    )
