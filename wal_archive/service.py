"""Recovery-point selection, verification, reporting and status."""

from __future__ import annotations

import hashlib
import json
import os
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Sequence

from . import errors
from .archive import (
    INSTANCE_RE,
    InProgressSegment,
    ScanConfig,
    ScanSnapshot,
    TimelineMarker,
    scan_archive,
)
from .errors import ArchiveError
from .reporting import (
    Aggregate,
    aggregate_state,
    prometheus_text,
    recovery_row_to_dict,
)
from .store import Store
from .wal import (
    parse_utc,
    segments_per_log,
    validate_segment,
)


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="microseconds").replace(
        "+00:00", "Z"
    )


def _canonical_target(value: str) -> str:
    if not isinstance(value, str):
        raise errors.invalid_request(
            "target_time must be an ISO-8601 timestamp",
            details={"target_time": str(value)},
        )
    try:
        return (
            parse_utc(value)
            .isoformat(timespec="microseconds")
            .replace("+00:00", "Z")
        )
    except (TypeError, ValueError) as exc:
        raise errors.invalid_request(
            "target_time must be an ISO-8601 timestamp",
            details={"target_time": value},
        ) from exc


def _request_key(instance: str, timeline: int, target_time: str) -> str:
    material = f"wal-archive-recovery/v1|{instance}|{timeline}|{target_time}"
    return "v1:" + hashlib.sha256(material.encode("utf-8")).hexdigest()


def _recovery_id(request_key: str) -> str:
    return "rp-" + hashlib.sha256(request_key.encode("utf-8")).hexdigest()[:20]


def _version_dict(row: Any, *, current: bool | None = None) -> dict[str, Any]:
    payload = {
        "archived_at": row["archived_at"],
        "checksum_pass_rate": round(float(row["checksum_pass_rate"]), 9),
        "classification": row["classification"],
        "conflict": bool(row["conflict"]),
        "digest": row["digest"],
        "filename": row["filename"],
        "header_ok": bool(row["header_ok"]),
        "instance": row["instance"],
        "length_ok": bool(row["length_ok"]),
        "pages_bad": int(row["pages_bad"]),
        "pages_checked": int(row["pages_checked"]),
        "pages_ok": int(row["pages_ok"]),
        "pages_parsed": int(row["pages_parsed"]),
        "pages_total": int(row["pages_total"]),
        "pages_unchecked": int(row["pages_unchecked"]),
        "reason": row["reason"],
        "rel_path": row["rel_path"],
        "segno": int(row["segno"]),
        "size": int(row["size"]),
        "timeline": int(row["timeline"]),
        "usable": bool(row["usable"]),
    }
    if current is not None:
        payload["current"] = current
    return payload


def _candidate_sort(row: Any) -> tuple[str, str, str]:
    return row["archived_at"], row["instance"], row["digest"]


class ArchiveService:
    def __init__(
        self,
        root: str | os.PathLike[str],
        db_path: str | os.PathLike[str],
        *,
        config: ScanConfig | None = None,
    ) -> None:
        self.root = Path(root)
        self.config = config or ScanConfig()
        try:
            segments_per_log(self.config.segment_size)
        except ValueError as exc:
            raise errors.invalid_request(
                "segment_size must be a positive divisor of 2^32"
            ) from exc
        self.store = Store(db_path)
        self._lock = threading.RLock()
        self.last_scan_summary: dict[str, Any] | None = None

    def close(self) -> None:
        self.store.close()

    @property
    def lock(self) -> threading.RLock:
        return self._lock

    def scan(self) -> dict[str, Any]:
        with self._lock:
            started_wall = _now_iso()
            started = time.monotonic()
            quarantines = self.store.get_quarantines()
            snapshot = scan_archive(
                self.root,
                config=self.config,
                quarantines=quarantines,
            )
            duration_ms = int((time.monotonic() - started) * 1000)
            scan_id = self.store.commit_scan(
                snapshot, started_at=started_wall, duration_ms=duration_ms
            )
            summary = {
                "archive_id": snapshot.archive_id,
                "duration_ms": duration_ms,
                "errors": [item.as_dict() for item in snapshot.errors],
                "files_seen": snapshot.files_seen,
                "in_progress": len(snapshot.in_progress),
                "markers": len(snapshot.markers),
                "root_label": snapshot.root_label,
                "scan_id": scan_id,
                "versions": len(snapshot.versions),
            }
            self.last_scan_summary = summary
            return summary

    def _current_rows(self, **kwargs: Any) -> list[Any]:
        return self.store.current_versions(**kwargs)

    def _instance_known(self, instance: str) -> bool:
        versions = self.store.current_versions(instance=instance)
        if versions:
            return True
        if any(
            item.instance == instance for item in self.store.current_in_progress()
        ):
            return True
        if self.store.all_versions(instance=instance):
            return True
        return bool(self.store.list_recoveries(instance=instance))

    def _instance_timelines(self, instance: str) -> set[int]:
        timelines = {
            int(row["timeline"]) for row in self.store.current_versions(instance=instance)
        }
        timelines |= {
            int(row["timeline"]) for row in self.store.all_versions(instance=instance)
        }
        timelines |= {
            int(row["timeline"])
            for row in self.store.list_recoveries(instance=instance)
        }
        for item in self.store.current_in_progress():
            if item.instance == instance and item.timeline is not None:
                timelines.add(int(item.timeline))
        return timelines

    def archive_view(
        self,
        *,
        instance: str | None = None,
        timeline: int | None = None,
    ) -> dict[str, Any]:
        with self._lock:
            if instance is not None and (
                not isinstance(instance, str) or not INSTANCE_RE.match(instance)
            ):
                raise errors.invalid_request("invalid instance identifier")
            rows = self.store.current_versions(instance=instance, timeline=timeline)
            if instance is not None and not rows and not self._instance_known(instance):
                raise errors.unknown_instance(instance)
            writing = [
                item.as_dict()
                for item in self.store.current_in_progress()
                if (instance is None or item.instance == instance)
                and (
                    timeline is None
                    or (item.timeline is not None and int(item.timeline) == timeline)
                )
            ]
            versions = [_version_dict(row) for row in rows]
            versions.sort(
                key=lambda item: (
                    item["instance"],
                    item["timeline"],
                    item["segno"],
                    item["digest"],
                )
            )
            markers = [
                marker.as_dict()
                for marker in self.store.current_markers()
                if timeline is None or marker.timeline == timeline
            ]
            markers.sort(
                key=lambda item: (
                    item["timeline"],
                    item["digest"],
                    item["rel_path"],
                )
            )
            scan = self.store.latest_scan()
            return {
                "archive_id": scan.archive_id if scan else None,
                "errors": [item.as_dict() for item in self.store.current_errors()],
                "in_progress": sorted(
                    writing, key=lambda item: (item["instance"], item["rel_path"])
                ),
                "markers": markers,
                "scan_id": scan.id if scan else None,
                "versions": versions,
            }

    def find_conflicts(
        self,
        *,
        timeline: int | None = None,
        segno: int | None = None,
    ) -> dict[str, Any]:
        with self._lock:
            rows = self.store.all_versions(timeline=timeline, segno=segno)
            active = self.store.latest_scan_id()
            groups: dict[tuple[int, int], list[Any]] = {}
            for row in rows:
                groups.setdefault((int(row["timeline"]), int(row["segno"])), []).append(row)
            output = []
            for (tli, seg), group in sorted(groups.items()):
                if len({row["digest"] for row in group}) < 2:
                    continue
                output.append(
                    {
                        "segno": seg,
                        "timeline": tli,
                        "versions": [
                            _version_dict(
                                row,
                                current=active is not None
                                and int(row["last_scan_id"]) == active,
                            )
                            for row in sorted(
                                group,
                                key=lambda item: (
                                    item["archived_at"],
                                    item["instance"],
                                    item["digest"],
                                ),
                            )
                        ],
                    }
                )
            return {"conflicts": output}

    def _effective_markers(self) -> dict[int, TimelineMarker]:
        return self.store.current_markers_effective()

    def _choose_version(
        self,
        candidates: Sequence[Any],
        *,
        marker: TimelineMarker | None,
        source_instance: str | None,
    ) -> tuple[Any, str, str | None]:
        ordered = sorted(candidates, key=_candidate_sort)
        if marker is not None and marker.authoritative_instance:
            for row in ordered:
                if row["instance"] == marker.authoritative_instance:
                    return (
                        row,
                        "timeline_marker_authoritative_instance",
                        marker.digest,
                    )
        if source_instance:
            for row in ordered:
                if row["instance"] == source_instance:
                    return row, "requested_source_instance", marker.digest if marker else None
        return row_first(ordered), "first_completed_valid", marker.digest if marker else None

    def _select_chain(
        self,
        *,
        instance: str,
        timeline: int,
        target_time: str,
        source_instance: str | None,
        refresh: bool,
    ) -> tuple[list[Any], dict[int, TimelineMarker], dict[str, Any]]:
        target_dt = parse_utc(target_time)
        rows = self.store.current_versions(timeline=timeline)
        if not rows:
            if not self._instance_known(instance):
                raise errors.unknown_instance(instance)
            if source_instance is not None and not self._instance_known(source_instance):
                raise errors.unknown_instance(source_instance)
            if timeline not in self._instance_timelines(instance):
                raise errors.unknown_timeline(instance, timeline)
            raise errors.no_recovery_point(instance, timeline, target_time)

        markers = self._effective_markers()
        marker = markers.get(timeline)
        complete = [row for row in rows if row["classification"] == "complete"]
        if not complete:
            raise errors.no_recovery_point(instance, timeline, target_time)

        eligible: list[Any] = []
        for row in complete:
            try:
                completed = parse_utc(row["archived_at"])
            except ValueError:
                continue
            if completed <= target_dt:
                eligible.append(row)
        if not eligible:
            raise errors.no_recovery_point(instance, timeline, target_time)

        by_segno: dict[int, list[Any]] = {}
        for row in eligible:
            by_segno.setdefault(int(row["segno"]), []).append(row)
        head_segno = max(by_segno)

        all_complete_segnos = {int(row["segno"]) for row in complete}
        lower_bound = min(all_complete_segnos)
        if marker is not None and marker.switch_segment is not None:
            lower_bound = max(1, int(marker.switch_segment))
        if lower_bound > head_segno:
            raise errors.no_recovery_point(instance, timeline, target_time)

        gaps = [
            segno
            for segno in range(lower_bound, head_segno + 1)
            if segno not in by_segno
        ]
        selection: list[dict[str, Any]] = []
        selected_rows: list[Any] = []
        for segno in range(lower_bound, head_segno + 1):
            candidates = by_segno.get(segno, [])
            if not candidates:
                continue
            chosen, reason, marker_digest = self._choose_version(
                candidates,
                marker=marker,
                source_instance=source_instance,
            )
            selected_rows.append(chosen)
            selection.append(
                {
                    "candidates": [
                        {
                            "archived_at": row["archived_at"],
                            "conflict": bool(row["conflict"]),
                            "digest": row["digest"],
                            "instance": row["instance"],
                            "size": int(row["size"]),
                        }
                        for row in sorted(candidates, key=_candidate_sort)
                    ],
                    "marker_digest": marker_digest,
                    "segno": segno,
                    "selection_reason": reason,
                    "selected_digest": chosen["digest"],
                    "selected_instance": chosen["instance"],
                }
            )
        trace = {
            "gaps": gaps,
            "head_segno": head_segno,
            "lower_bound": lower_bound,
            "marker": marker.as_dict() if marker else None,
            "selection": selection,
        }
        if gaps:
            raise errors.recovery_gap(gaps, instance, timeline)
        return selected_rows, markers, trace

    def _safe_segment_path(self, rel_path: str) -> Path:
        root = self.root.resolve()
        path = (root / rel_path).resolve()
        try:
            path.relative_to(root)
        except ValueError as exc:
            code, name = errors.E_UNSAFE_PATH
            raise errors.err(
                code,
                name,
                "archive path escapes the configured root",
                rel_path=rel_path,
            ) from exc
        if not path.is_file() or path.is_symlink():
            code, name = errors.E_SEGMENT_READ_ERROR
            raise errors.err(
                code,
                name,
                "selected segment is missing or is not a regular file",
                rel_path=rel_path,
            )
        return path

    def _verify_chain(
        self, selected_rows: Sequence[Any]
    ) -> tuple[list[dict[str, Any]], dict[str, Any], list[dict[str, Any]]]:
        chain: list[dict[str, Any]] = []
        validation_rows: list[dict[str, Any]] = []
        failures: list[dict[str, Any]] = []
        pages_total = pages_checked = pages_ok = pages_bad = 0

        for row in selected_rows:
            rel_path = row["rel_path"]
            path = self._safe_segment_path(rel_path)
            try:
                validation = validate_segment(
                    path,
                    expected_size=self.config.segment_size,
                    expected_timeline=int(row["timeline"]),
                    expected_segno=int(row["segno"]),
                )
            except ArchiveError as exc:
                failures.append(
                    {
                        "code": exc.code,
                        "digest": row["digest"],
                        "instance": row["instance"],
                        "message": exc.message,
                        "rel_path": rel_path,
                        "segno": int(row["segno"]),
                    }
                )
                continue
            if validation.digest != row["digest"]:
                failures.append(
                    {
                        "code": "WAL014",
                        "digest": row["digest"],
                        "actual_digest": validation.digest,
                        "instance": row["instance"],
                        "message": "segment content changed after the archive scan",
                        "rel_path": rel_path,
                        "segno": int(row["segno"]),
                    }
                )
                continue
            if validation.classification != "complete":
                failures.append(
                    {
                        "code": "WAL014",
                        "digest": row["digest"],
                        "instance": row["instance"],
                        "message": validation.reason,
                        "rel_path": rel_path,
                        "segno": int(row["segno"]),
                    }
                )
                continue
            pages_total += validation.pages_total
            pages_checked += validation.pages_checked
            pages_ok += validation.pages_ok
            pages_bad += validation.pages_bad
            item = {
                "filename": row["filename"],
                "segno": int(row["segno"]),
                "selected_version": {
                    "archived_at": row["archived_at"],
                    "conflict": bool(row["conflict"]),
                    "digest": row["digest"],
                    "instance": row["instance"],
                    "rel_path": rel_path,
                    "size": int(row["size"]),
                },
                "timeline": int(row["timeline"]),
                "validation": {
                    "checksum_pass_rate": round(validation.checksum_pass_rate, 9),
                    "classification": validation.classification,
                    "pages_bad": validation.pages_bad,
                    "pages_checked": validation.pages_checked,
                    "pages_ok": validation.pages_ok,
                    "pages_total": validation.pages_total,
                    "reason": validation.reason,
                },
            }
            chain.append(item)
            validation_rows.append(
                {
                    "checksum_pass_rate": validation.checksum_pass_rate,
                    "classification": validation.classification,
                    "digest": row["digest"],
                    "instance": row["instance"],
                    "pages_bad": validation.pages_bad,
                    "pages_checked": validation.pages_checked,
                    "pages_ok": validation.pages_ok,
                    "pages_total": validation.pages_total,
                    "segno": int(row["segno"]),
                    "timeline": int(row["timeline"]),
                    "details": {
                        "reason": validation.reason,
                        "rel_path": rel_path,
                    },
                }
            )

        if failures or len(chain) != len(selected_rows):
            code, name = errors.E_RECOVERY_VERIFY_FAILED
            raise errors.err(
                code,
                name,
                "one or more recovery chain segments failed live validation",
                http_status=409,
                details={"failures": failures},
            )

        pass_rate = float(pages_ok) / float(pages_checked) if pages_checked else 0.0
        summary = {
            "checksum_pass_rate": round(pass_rate, 9),
            "classification": "complete",
            "pages_bad": pages_bad,
            "pages_checked": pages_checked,
            "pages_ok": pages_ok,
            "pages_total": pages_total,
            "segments_verified": len(chain),
        }
        return chain, summary, validation_rows

    def resolve_recovery(
        self,
        *,
        instance: str,
        timeline: int,
        target_time: str,
        source_instance: str | None = None,
        refresh: bool = True,
    ) -> dict[str, Any]:
        with self._lock:
            if not isinstance(instance, str) or not INSTANCE_RE.match(instance):
                raise errors.invalid_request("invalid instance identifier")
            if isinstance(timeline, bool) or not isinstance(timeline, int) or timeline <= 0:
                raise errors.invalid_request("timeline must be a positive integer")
            if source_instance is not None and (
                not isinstance(source_instance, str)
                or not INSTANCE_RE.match(source_instance)
            ):
                raise errors.invalid_request("invalid source_instance identifier")
            target = _canonical_target(target_time)
            request_key = _request_key(instance, timeline, target)
            existing = self.store.recovery_by_request(request_key)
            if existing is not None:
                return {
                    "recovery_point": recovery_row_to_dict(existing),
                    "reused": True,
                }

            if refresh:
                self.scan()
            if not self._instance_known(instance):
                raise errors.unknown_instance(instance)
            if timeline not in self._instance_timelines(instance):
                raise errors.unknown_timeline(instance, timeline)

            selected_rows, markers, trace = self._select_chain(
                instance=instance,
                timeline=timeline,
                target_time=target,
                source_instance=source_instance,
                refresh=refresh,
            )
            chain, validation, validation_rows = self._verify_chain(selected_rows)
            scan = self.store.latest_scan()
            recovery_id = _recovery_id(request_key)
            saved, created = self.store.commit_recovery(
                recovery_id=recovery_id,
                request_key=request_key,
                instance=instance,
                timeline=timeline,
                target_time=target,
                source_instance=source_instance,
                head_segno=trace["head_segno"],
                chain=chain,
                gaps=[],
                validation=validation,
                selection=trace["selection"],
                archive_id=scan.archive_id if scan else "",
                validation_rows=validation_rows,
            )
            return {
                "recovery_point": recovery_row_to_dict(saved),
                "reused": not created,
            }

    def get_recovery(self, recovery_id: str) -> dict[str, Any]:
        with self._lock:
            row = self.store.recovery_by_id(recovery_id)
            if row is None:
                code, name = errors.E_NOT_FOUND
                raise errors.err(
                    code, name, "recovery point not found", http_status=404
                )
            return {"recovery_point": recovery_row_to_dict(row)}

    def list_recoveries(
        self, *, instance: str | None = None, timeline: int | None = None
    ) -> dict[str, Any]:
        with self._lock:
            rows = self.store.list_recoveries(instance=instance, timeline=timeline)
            return {"recovery_points": [recovery_row_to_dict(row) for row in rows]}

    def validate_segment(
        self,
        *,
        instance: str,
        timeline: int,
        segno: int,
        digest: str | None = None,
        refresh: bool = True,
    ) -> dict[str, Any]:
        with self._lock:
            if refresh:
                self.scan()
            rows = [
                row
                for row in self.store.current_versions(
                    instance=instance, timeline=timeline
                )
                if int(row["segno"]) == int(segno)
                and (digest is None or row["digest"] == digest)
            ]
            if not rows:
                code, name = errors.E_NOT_FOUND
                raise errors.err(
                    code,
                    name,
                    "no matching WAL segment version in the current view",
                    http_status=404,
                    details={
                        "instance": instance,
                        "segno": segno,
                        "timeline": timeline,
                    },
                )
            output = []
            for row in rows:
                path = self._safe_segment_path(row["rel_path"])
                validation = validate_segment(
                    path,
                    expected_size=self.config.segment_size,
                    expected_timeline=timeline,
                    expected_segno=segno,
                )
                self.store.record_validation(
                    instance=instance,
                    timeline=timeline,
                    segno=segno,
                    digest=validation.digest,
                    classification=validation.classification,
                    pages_total=validation.pages_total,
                    pages_checked=validation.pages_checked,
                    pages_ok=validation.pages_ok,
                    pages_bad=validation.pages_bad,
                    checksum_pass_rate=validation.checksum_pass_rate,
                    source="manual",
                    details={
                        "reason": validation.reason,
                        "rel_path": row["rel_path"],
                    },
                )
                output.append(
                    {
                        "classification": validation.classification,
                        "digest": validation.digest,
                        "matches_catalog_digest": validation.digest == row["digest"],
                        "pages_bad": validation.pages_bad,
                        "pages_checked": validation.pages_checked,
                        "pages_ok": validation.pages_ok,
                        "pages_total": validation.pages_total,
                        "reason": validation.reason,
                        "rel_path": row["rel_path"],
                        "selected_version": _version_dict(row),
                    }
                )
            return {"validations": output}

    def status(self, *, instance: str, timeline: int | None = None) -> dict[str, Any]:
        with self._lock:
            if not isinstance(instance, str) or not INSTANCE_RE.match(instance):
                raise errors.invalid_request("invalid instance identifier")
            if timeline is not None and (
                isinstance(timeline, bool) or not isinstance(timeline, int) or timeline <= 0
            ):
                raise errors.invalid_request("timeline must be a positive integer")
            if not self._instance_known(instance):
                raise errors.unknown_instance(instance)
            timelines = self._instance_timelines(instance)
            latest_recovery = self.store.latest_recovery(
                instance=instance, timeline=timeline
            )
            if timeline is None:
                if latest_recovery is not None:
                    timeline = int(latest_recovery["timeline"])
                elif timelines:
                    timeline = max(timelines)
                else:
                    timeline = 0
            versions = self.store.current_versions(instance=instance, timeline=timeline)
            in_progress = [
                item
                for item in self.store.current_in_progress()
                if item.instance == instance
                and (item.timeline is not None and int(item.timeline) == timeline)
            ]
            recoveries = [
                row
                for row in self.store.list_recoveries(instance=instance, timeline=timeline)
            ]
            aggregates, _, _ = aggregate_state(
                versions, in_progress, recoveries
            )
            agg = aggregates.get((instance, timeline), Aggregate())
            counts = agg.as_dict()
            latest_validation = self.store.latest_validation(
                instance=instance, timeline=timeline
            )
            validation_summary = None
            if latest_validation is not None:
                details = {}
                try:
                    details = json.loads(latest_validation["details_json"])
                except (TypeError, ValueError):
                    details = {}
                validation_summary = {
                    "checksum_pass_rate": round(
                        float(latest_validation["checksum_pass_rate"]), 9
                    ),
                    "classification": latest_validation["classification"],
                    "digest": latest_validation["digest"],
                    "pages_bad": int(latest_validation["pages_bad"]),
                    "pages_checked": int(latest_validation["pages_checked"]),
                    "pages_ok": int(latest_validation["pages_ok"]),
                    "reason": details.get("reason"),
                    "segno": int(latest_validation["segno"]),
                    "source": latest_validation["source"],
                }
            current = None
            if latest_recovery is not None:
                current = {
                    "archive_id": latest_recovery["archive_id"],
                    "chain_length": int(latest_recovery["chain_length"]),
                    "gaps": json.loads(latest_recovery["gaps_json"]),
                    "head_segno": int(latest_recovery["head_segno"]),
                    "id": latest_recovery["id"],
                    "target_time": latest_recovery["target_time"],
                }
            return {
                "chain_length": int(latest_recovery["chain_length"])
                if latest_recovery is not None
                else 0,
                "counts": {
                    "checksum_failed_segments": counts["checksum_failed_segments"],
                    "conflict_versions": counts["conflict_versions"],
                    "gap_segment_numbers": counts["gap_segment_numbers"],
                    "segment_count": counts["segment_count"],
                    "truncated_segments": counts["truncated_segments"],
                    "usable_bytes": counts["usable_bytes"],
                    "writing_segments": counts["writing_segments"],
                },
                "current_recovery_point": current,
                "instance": instance,
                "latest_validation": validation_summary,
                "timeline": timeline,
            }

    def _report_payload(self) -> dict[str, Any]:
        started = time.monotonic()
        scan = self.store.latest_scan()
        versions = self.store.current_versions()
        in_progress = self.store.current_in_progress()
        recoveries = self.store.list_recoveries()
        aggregates, instances, _ = aggregate_state(versions, in_progress, recoveries)
        by_instance: dict[str, list[dict[str, Any]]] = {name: [] for name in instances}
        for instance, timeline in sorted(aggregates, key=lambda key: (key[0], key[1])):
            agg = aggregates[(instance, timeline)]
            timeline_payload = agg.as_dict()
            timeline_payload["committed_recovery_points"] = agg.recovery_count
            timeline_payload["timeline"] = timeline
            by_instance.setdefault(instance, []).append(timeline_payload)
        instances_payload = [
            {
                "instance": instance,
                "timelines": sorted(
                    by_instance.get(instance, []),
                    key=lambda item: item["timeline"],
                ),
            }
            for instance in sorted(by_instance)
        ]
        recovery_payload = [recovery_row_to_dict(row) for row in recoveries]
        recovery_payload.sort(
            key=lambda item: (
                item["instance"],
                item["timeline"],
                item["target_time"],
                item["id"],
            )
        )
        errors_payload = [item.as_dict() for item in self.store.current_errors()]
        errors_payload.sort(
            key=lambda item: (item["code"], item["path"], item["message"])
        )
        writing_payload = [item.as_dict() for item in in_progress]
        writing_payload.sort(
            key=lambda item: (
                item["instance"],
                item["rel_path"],
                item["reason"],
            )
        )
        markers_payload = [item.as_dict() for item in self.store.current_markers()]
        markers_payload.sort(
            key=lambda item: (item["timeline"], item["digest"], item["rel_path"])
        )
        duration_ms = int((time.monotonic() - started) * 1000)
        return {
            "archive": {
                "archive_id": scan.archive_id if scan else None,
                "files_seen": scan.segment_files if scan else 0,
                "markers": len(markers_payload),
                "root_label": scan.root_label if scan else self.root.name,
                "versions": len(versions),
            },
            "duration_ms": duration_ms,
            "errors": errors_payload,
            "generated_at": _now_iso(),
            "instances": instances_payload,
            "markers": markers_payload,
            "recovery_points": recovery_payload,
            "writing": writing_payload,
        }

    def report(self) -> dict[str, Any]:
        with self._lock:
            return self._report_payload()

    def metrics(self) -> str:
        with self._lock:
            versions = self.store.current_versions()
            in_progress = self.store.current_in_progress()
            recoveries = self.store.list_recoveries()
            aggregates, _, _ = aggregate_state(versions, in_progress, recoveries)
            return prometheus_text(
                aggregates, scan_errors=len(self.store.current_errors())
            )


def row_first(rows: Sequence[Any]) -> Any:
    if not rows:
        raise RuntimeError("cannot choose from an empty candidate list")
    return rows[0]
