from __future__ import annotations

import json
import os
import sqlite3
import threading
from datetime import datetime, timezone
from pathlib import Path


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _json(value) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def _duration_seconds(first_seen: str, now: str) -> int:
    try:
        first = datetime.fromisoformat(first_seen.replace("Z", "+00:00"))
        last = datetime.fromisoformat(now.replace("Z", "+00:00"))
        return max(0, int(round((last - first).total_seconds())))
    except ValueError:
        return 0


SCHEMA = """
CREATE TABLE IF NOT EXISTS schema_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS inspections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    rack_id TEXT NOT NULL,
    status TEXT NOT NULL,
    conclusion TEXT,
    started_at TEXT NOT NULL,
    completed_at TEXT,
    config_fingerprint TEXT NOT NULL,
    config_version INTEGER NOT NULL,
    expected_devices INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS inspections_rack_status
    ON inspections(rack_id, status, id DESC);
CREATE TABLE IF NOT EXISTS inspection_devices (
    inspection_id INTEGER NOT NULL REFERENCES inspections(id),
    device_id TEXT NOT NULL,
    rack_id TEXT NOT NULL,
    firmware_profile TEXT NOT NULL,
    firmware_version TEXT NOT NULL,
    status TEXT NOT NULL,
    primary_protocol TEXT,
    protocols_used_json TEXT NOT NULL,
    fallback_used INTEGER NOT NULL,
    power_state TEXT NOT NULL,
    unparsable_count INTEGER NOT NULL,
    duration_ms REAL NOT NULL,
    asset_json TEXT NOT NULL,
    normalized_json TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY(inspection_id, device_id)
);
CREATE TABLE IF NOT EXISTS protocol_results (
    inspection_id INTEGER NOT NULL REFERENCES inspections(id),
    device_id TEXT NOT NULL,
    protocol TEXT NOT NULL,
    success INTEGER NOT NULL,
    usable INTEGER NOT NULL,
    error TEXT NOT NULL,
    duration_ms REAL NOT NULL,
    raw_json TEXT NOT NULL,
    normalized_json TEXT NOT NULL,
    PRIMARY KEY(inspection_id, device_id, protocol)
);
CREATE TABLE IF NOT EXISTS unparsable_fields (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    inspection_id INTEGER NOT NULL REFERENCES inspections(id),
    device_id TEXT NOT NULL,
    protocol TEXT NOT NULL,
    path TEXT NOT NULL,
    raw_snippet TEXT NOT NULL,
    reason TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS unparsable_inspection
    ON unparsable_fields(inspection_id, device_id);
CREATE TABLE IF NOT EXISTS inconsistencies (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    inspection_id INTEGER NOT NULL REFERENCES inspections(id),
    device_id TEXT NOT NULL,
    item TEXT NOT NULL,
    kind TEXT NOT NULL,
    ipmi_value TEXT NOT NULL,
    ipmi_raw TEXT NOT NULL,
    redfish_value TEXT NOT NULL,
    redfish_raw TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS inconsistencies_inspection
    ON inconsistencies(inspection_id, device_id);
CREATE TABLE IF NOT EXISTS sensor_observations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    inspection_id INTEGER NOT NULL REFERENCES inspections(id),
    device_id TEXT NOT NULL,
    sensor_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    value REAL NOT NULL,
    unit TEXT NOT NULL,
    lower_bound REAL NOT NULL,
    upper_bound REAL NOT NULL,
    source_protocol TEXT NOT NULL,
    raw_snippet TEXT NOT NULL,
    observed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS sensor_series
    ON sensor_observations(device_id, sensor_name, observed_at, id);
CREATE TABLE IF NOT EXISTS violations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT NOT NULL,
    sensor_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    rule TEXT NOT NULL,
    threshold_value REAL NOT NULL,
    first_seen TEXT NOT NULL,
    last_seen TEXT NOT NULL,
    last_value REAL NOT NULL,
    duration_seconds INTEGER NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('active', 'resolved')),
    last_inspection_id INTEGER NOT NULL,
    resolved_at TEXT,
    resolution_inspection_id INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS violations_one_active
    ON violations(device_id, sensor_name) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS violations_device_status
    ON violations(device_id, status);
CREATE TABLE IF NOT EXISTS violation_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    violation_id INTEGER NOT NULL REFERENCES violations(id),
    inspection_id INTEGER NOT NULL REFERENCES inspections(id),
    device_id TEXT NOT NULL,
    sensor_name TEXT NOT NULL,
    event_type TEXT NOT NULL CHECK(event_type IN ('opened', 'ongoing', 'resolved')),
    observed_at TEXT NOT NULL,
    value REAL NOT NULL,
    duration_seconds INTEGER NOT NULL,
    UNIQUE(violation_id, inspection_id, event_type)
);
CREATE INDEX IF NOT EXISTS violation_events_inspection
    ON violation_events(inspection_id, device_id);
CREATE TABLE IF NOT EXISTS violation_resolutions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    violation_id INTEGER NOT NULL UNIQUE REFERENCES violations(id),
    inspection_id INTEGER NOT NULL REFERENCES inspections(id),
    device_id TEXT NOT NULL,
    sensor_name TEXT NOT NULL,
    resolved_at TEXT NOT NULL,
    last_value REAL NOT NULL,
    duration_seconds INTEGER NOT NULL,
    UNIQUE(violation_id)
);
"""


class RackStorage:
    def __init__(self, path: str | os.PathLike):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self.connection = sqlite3.connect(
            str(self.path), timeout=5.0, check_same_thread=False, isolation_level=None
        )
        self.connection.row_factory = sqlite3.Row
        with self._lock:
            self.connection.execute("PRAGMA journal_mode=WAL")
            self.connection.execute("PRAGMA synchronous=NORMAL")
            self.connection.execute("PRAGMA foreign_keys=ON")
            self.connection.execute("PRAGMA busy_timeout=5000")
            self.connection.executescript(SCHEMA)
            self.connection.execute(
                "INSERT OR REPLACE INTO schema_meta(key, value) VALUES('schema_version', '1')"
            )

    def close(self) -> None:
        with self._lock:
            self.connection.close()

    def _fetchone(self, sql: str, params=()):
        with self._lock:
            return self.connection.execute(sql, params).fetchone()

    def _fetchall(self, sql: str, params=()):
        with self._lock:
            return self.connection.execute(sql, params).fetchall()

    def start_or_resume_inspection(
        self,
        *,
        rack_id: str,
        fingerprint: str,
        config_version: int,
        expected_devices: int,
    ) -> dict:
        with self._lock:
            row = self.connection.execute(
                "SELECT * FROM inspections WHERE rack_id=? AND status='running' "
                "ORDER BY id DESC LIMIT 1",
                (rack_id,),
            ).fetchone()
            if row is not None and row["config_fingerprint"] == fingerprint:
                committed = {
                    item["device_id"]
                    for item in self.connection.execute(
                        "SELECT device_id FROM inspection_devices WHERE inspection_id=?",
                        (row["id"],),
                    )
                }
                return {
                    "inspection_id": row["id"],
                    "resumed": True,
                    "committed_device_ids": sorted(committed),
                    "started_at": row["started_at"],
                }
            now = utc_now()
            if row is not None:
                self.connection.execute(
                    "UPDATE inspections SET status='completed', conclusion='interrupted', "
                    "completed_at=? WHERE id=?",
                    (now, row["id"]),
                )
            cursor = self.connection.execute(
                "INSERT INTO inspections(rack_id, status, conclusion, started_at, "
                "config_fingerprint, config_version, expected_devices) "
                "VALUES(?, 'running', NULL, ?, ?, ?, ?)",
                (rack_id, now, fingerprint, config_version, expected_devices),
            )
            return {
                "inspection_id": cursor.lastrowid,
                "resumed": False,
                "committed_device_ids": [],
                "started_at": now,
            }

    def committed_device_ids(self, inspection_id: int) -> set[str]:
        rows = self._fetchall(
            "SELECT device_id FROM inspection_devices WHERE inspection_id=?",
            (inspection_id,),
        )
        return {row["device_id"] for row in rows}

    def record_device_result(
        self,
        inspection_id: int,
        aggregate: dict,
        *,
        observed_at: str | None = None,
    ) -> None:
        observed_at = observed_at or utc_now()
        device_id = aggregate["device_id"]
        with self._lock:
            self.connection.execute("BEGIN IMMEDIATE")
            try:
                self.connection.execute(
                    "INSERT INTO inspection_devices("
                    "inspection_id, device_id, rack_id, firmware_profile, firmware_version, "
                    "status, primary_protocol, protocols_used_json, fallback_used, "
                    "power_state, unparsable_count, duration_ms, asset_json, "
                    "normalized_json, created_at) "
                    "VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                    (
                        inspection_id,
                        device_id,
                        aggregate["rack_id"],
                        aggregate["firmware_profile"],
                        aggregate["firmware_version"],
                        aggregate["status"],
                        aggregate["primary_protocol"],
                        _json(aggregate["protocols_used"]),
                        1 if aggregate["fallback_used"] else 0,
                        aggregate["power"]["value"],
                        aggregate["unparsable_count"],
                        float(aggregate.get("duration_ms", 0.0)),
                        _json(aggregate["asset"]),
                        _json(
                            {
                                key: value
                                for key, value in aggregate.items()
                                if key != "protocols"
                            }
                        ),
                        observed_at,
                    ),
                )
                for protocol, result in sorted(aggregate["protocols"].items()):
                    normalized_result = {
                        key: value for key, value in result.items() if key != "raw_response"
                    }
                    self.connection.execute(
                        "INSERT INTO protocol_results("
                        "inspection_id, device_id, protocol, success, usable, error, "
                        "duration_ms, raw_json, normalized_json) VALUES(?,?,?,?,?,?,?,?,?)",
                        (
                            inspection_id,
                            device_id,
                            protocol,
                            1 if result.get("success") else 0,
                            1 if result.get("usable") else 0,
                            result.get("error", ""),
                            float(result.get("duration_ms", 0.0)),
                            _json(result.get("raw_response", {})),
                            _json(normalized_result),
                        ),
                    )
                for item in aggregate.get("unparsable", []):
                    self.connection.execute(
                        "INSERT INTO unparsable_fields("
                        "inspection_id, device_id, protocol, path, raw_snippet, reason) "
                        "VALUES(?,?,?,?,?,?)",
                        (
                            inspection_id,
                            device_id,
                            item["protocol"],
                            item["path"],
                            item["raw_snippet"],
                            item["reason"],
                        ),
                    )
                for item in aggregate.get("inconsistencies", []):
                    self.connection.execute(
                        "INSERT INTO inconsistencies("
                        "inspection_id, device_id, item, kind, ipmi_value, ipmi_raw, "
                        "redfish_value, redfish_raw) VALUES(?,?,?,?,?,?,?,?)",
                        (
                            inspection_id,
                            device_id,
                            item["item"],
                            item["kind"],
                            _json(item["ipmi"]["value"]),
                            _json(item["ipmi"]["raw"]),
                            _json(item["redfish"]["value"]),
                            _json(item["redfish"]["raw"]),
                        ),
                    )
                for sensor in aggregate.get("sensors", []):
                    self.connection.execute(
                        "INSERT INTO sensor_observations("
                        "inspection_id, device_id, sensor_name, kind, value, unit, "
                        "lower_bound, upper_bound, source_protocol, raw_snippet, observed_at) "
                        "VALUES(?,?,?,?,?,?,?,?,?,?,?)",
                        (
                            inspection_id,
                            device_id,
                            sensor["name"],
                            sensor["kind"],
                            float(sensor["value"]),
                            sensor["unit"],
                            float(sensor["lower"]),
                            float(sensor["upper"]),
                            sensor["source_protocol"],
                            sensor["raw"],
                            observed_at,
                        ),
                    )
                    self._apply_compliance(
                        inspection_id,
                        device_id,
                        sensor,
                        observed_at,
                    )
                self.connection.execute("COMMIT")
            except Exception:
                self.connection.execute("ROLLBACK")
                raise

    def _apply_compliance(
        self,
        inspection_id: int,
        device_id: str,
        sensor: dict,
        observed_at: str,
    ) -> None:
        value = float(sensor["value"])
        lower = float(sensor["lower"])
        upper = float(sensor["upper"])
        violated = value < lower or value > upper
        rule = None
        threshold = None
        if value < lower:
            rule = f"{sensor['kind']}_lower"
            threshold = lower
        elif value > upper:
            rule = f"{sensor['kind']}_upper"
            threshold = upper
        active = self.connection.execute(
            "SELECT * FROM violations WHERE device_id=? AND sensor_name=? "
            "AND status='active' ORDER BY id DESC LIMIT 1",
            (device_id, sensor["name"]),
        ).fetchone()
        if violated:
            if active is None:
                cursor = self.connection.execute(
                    "INSERT INTO violations("
                    "device_id, sensor_name, kind, rule, threshold_value, first_seen, "
                    "last_seen, last_value, duration_seconds, status, last_inspection_id) "
                    "VALUES(?,?,?,?,?,?,?,?,?, 'active', ?)",
                    (
                        device_id,
                        sensor["name"],
                        sensor["kind"],
                        rule,
                        threshold,
                        observed_at,
                        observed_at,
                        value,
                        0,
                        inspection_id,
                    ),
                )
                violation_id = cursor.lastrowid
                self.connection.execute(
                    "INSERT OR IGNORE INTO violation_events("
                    "violation_id, inspection_id, device_id, sensor_name, event_type, "
                    "observed_at, value, duration_seconds) VALUES(?,?,?,?,?,?,?,?)",
                    (
                        violation_id,
                        inspection_id,
                        device_id,
                        sensor["name"],
                        "opened",
                        observed_at,
                        value,
                        0,
                    ),
                )
            else:
                duration = _duration_seconds(active["first_seen"], observed_at)
                self.connection.execute(
                    "UPDATE violations SET last_seen=?, last_value=?, duration_seconds=?, "
                    "last_inspection_id=?, rule=?, threshold_value=? WHERE id=?",
                    (
                        observed_at,
                        value,
                        duration,
                        inspection_id,
                        rule,
                        threshold,
                        active["id"],
                    ),
                )
                self.connection.execute(
                    "INSERT OR IGNORE INTO violation_events("
                    "violation_id, inspection_id, device_id, sensor_name, event_type, "
                    "observed_at, value, duration_seconds) VALUES(?,?,?,?,?,?,?,?)",
                    (
                        active["id"],
                        inspection_id,
                        device_id,
                        sensor["name"],
                        "ongoing",
                        observed_at,
                        value,
                        duration,
                    ),
                )
        elif active is not None:
            duration = _duration_seconds(active["first_seen"], observed_at)
            self.connection.execute(
                "UPDATE violations SET status='resolved', last_seen=?, last_value=?, "
                "duration_seconds=?, resolved_at=?, resolution_inspection_id=? WHERE id=?",
                (
                    observed_at,
                    value,
                    duration,
                    observed_at,
                    inspection_id,
                    active["id"],
                ),
            )
            self.connection.execute(
                "INSERT OR IGNORE INTO violation_events("
                "violation_id, inspection_id, device_id, sensor_name, event_type, "
                "observed_at, value, duration_seconds) VALUES(?,?,?,?,?,?,?,?)",
                (
                    active["id"],
                    inspection_id,
                    device_id,
                    sensor["name"],
                    "resolved",
                    observed_at,
                    value,
                    duration,
                ),
            )
            self.connection.execute(
                "INSERT OR IGNORE INTO violation_resolutions("
                "violation_id, inspection_id, device_id, sensor_name, resolved_at, "
                "last_value, duration_seconds) VALUES(?,?,?,?,?,?,?)",
                (
                    active["id"],
                    inspection_id,
                    device_id,
                    sensor["name"],
                    observed_at,
                    value,
                    duration,
                ),
            )

    def complete_inspection(self, inspection_id: int) -> dict:
        with self._lock:
            self.connection.execute("BEGIN IMMEDIATE")
            try:
                rows = self.connection.execute(
                    "SELECT status FROM inspection_devices WHERE inspection_id=?",
                    (inspection_id,),
                ).fetchall()
                event_counts = self.connection.execute(
                    "SELECT event_type, COUNT(*) AS count FROM violation_events "
                    "WHERE inspection_id=? GROUP BY event_type",
                    (inspection_id,),
                ).fetchall()
                active_count = self.connection.execute(
                    "SELECT COUNT(*) AS count FROM violations WHERE status='active'"
                ).fetchone()["count"]
                events = {row["event_type"]: row["count"] for row in event_counts}
                if any(row["status"] == "failed" for row in rows):
                    conclusion = "failed"
                elif active_count > 0 or events.get("opened", 0) or events.get("ongoing", 0):
                    conclusion = "non_compliant"
                elif any(row["status"] == "degraded" for row in rows):
                    conclusion = "degraded"
                else:
                    conclusion = "compliant"
                completed = utc_now()
                self.connection.execute(
                    "UPDATE inspections SET status='completed', conclusion=?, completed_at=? "
                    "WHERE id=?",
                    (conclusion, completed, inspection_id),
                )
                self.connection.execute("COMMIT")
                return {"conclusion": conclusion, "completed_at": completed}
            except Exception:
                self.connection.execute("ROLLBACK")
                raise

    def latest_status(self, rack_id: str) -> dict:
        with self._lock:
            inspection = self.connection.execute(
                "SELECT * FROM inspections WHERE rack_id=? ORDER BY id DESC LIMIT 1",
                (rack_id,),
            ).fetchone()
            if inspection is None:
                return {
                    "rack_id": rack_id,
                    "inspection": None,
                    "devices": [],
                    "active_violations": [],
                    "inconsistencies": [],
                    "unparsable": [],
                }
            inspection_id = inspection["id"]
            device_rows = self.connection.execute(
                "SELECT * FROM inspection_devices WHERE inspection_id=? ORDER BY device_id",
                (inspection_id,),
            ).fetchall()
            active_by_device = {
                row["device_id"]: row["count"]
                for row in self.connection.execute(
                    "SELECT device_id, COUNT(*) AS count FROM violations "
                    "WHERE status='active' GROUP BY device_id"
                )
            }
            events_by_device: dict[str, dict[str, int]] = {}
            for row in self.connection.execute(
                "SELECT device_id, event_type, COUNT(*) AS count FROM violation_events "
                "WHERE inspection_id=? GROUP BY device_id, event_type",
                (inspection_id,),
            ):
                events_by_device.setdefault(row["device_id"], {})[row["event_type"]] = row["count"]
            devices = []
            for row in device_rows:
                normalized = json.loads(row["normalized_json"])
                event_summary = events_by_device.get(row["device_id"], {})
                devices.append(
                    {
                        "device_id": row["device_id"],
                        "firmware_profile": row["firmware_profile"],
                        "firmware_version": row["firmware_version"],
                        "status": row["status"],
                        "primary_protocol": row["primary_protocol"],
                        "protocols_used": json.loads(row["protocols_used_json"]),
                        "fallback_used": bool(row["fallback_used"]),
                        "power_state": row["power_state"],
                        "unparsable_fields": row["unparsable_count"],
                        "asset": json.loads(row["asset_json"]),
                        "violation_summary": {
                            "active": int(active_by_device.get(row["device_id"], 0)),
                            "opened": int(event_summary.get("opened", 0)),
                            "ongoing": int(event_summary.get("ongoing", 0)),
                            "resolved": int(event_summary.get("resolved", 0)),
                        },
                        "sensor_count": len(normalized.get("sensors", [])),
                    }
                )
            active_violations = [
                dict(row)
                for row in self.connection.execute(
                    "SELECT id, device_id, sensor_name, kind, rule, threshold_value, "
                    "first_seen, last_seen, last_value, duration_seconds, status "
                    "FROM violations WHERE status='active' ORDER BY device_id, sensor_name"
                )
            ]
            inconsistencies = [
                {
                    "device_id": row["device_id"],
                    "item": row["item"],
                    "kind": row["kind"],
                    "ipmi": {
                        "value": json.loads(row["ipmi_value"]),
                        "raw": json.loads(row["ipmi_raw"]),
                    },
                    "redfish": {
                        "value": json.loads(row["redfish_value"]),
                        "raw": json.loads(row["redfish_raw"]),
                    },
                }
                for row in self.connection.execute(
                    "SELECT * FROM inconsistencies WHERE inspection_id=? "
                    "ORDER BY device_id, item",
                    (inspection_id,),
                )
            ]
            unparsable = [
                {
                    "device_id": row["device_id"],
                    "protocol": row["protocol"],
                    "path": row["path"],
                    "raw_snippet": row["raw_snippet"],
                    "reason": row["reason"],
                }
                for row in self.connection.execute(
                    "SELECT device_id, protocol, path, raw_snippet, reason "
                    "FROM unparsable_fields WHERE inspection_id=? "
                    "ORDER BY device_id, protocol, path",
                    (inspection_id,),
                )
            ]
            return {
                "rack_id": rack_id,
                "inspection": {
                    "id": inspection_id,
                    "status": inspection["status"],
                    "conclusion": inspection["conclusion"],
                    "started_at": inspection["started_at"],
                    "completed_at": inspection["completed_at"],
                },
                "devices": devices,
                "active_violations": active_violations,
                "inconsistencies": inconsistencies,
                "unparsable": unparsable,
            }

    def inspection_detail(self, inspection_id: int) -> dict | None:
        with self._lock:
            inspection = self.connection.execute(
                "SELECT * FROM inspections WHERE id=?", (inspection_id,)
            ).fetchone()
            if inspection is None:
                return None
            devices = []
            rows = self.connection.execute(
                "SELECT * FROM inspection_devices WHERE inspection_id=? ORDER BY device_id",
                (inspection_id,),
            ).fetchall()
            for row in rows:
                protocol_rows = self.connection.execute(
                    "SELECT protocol, success, usable, error, duration_ms, raw_json, "
                    "normalized_json FROM protocol_results "
                    "WHERE inspection_id=? AND device_id=? ORDER BY protocol",
                    (inspection_id, row["device_id"]),
                ).fetchall()
                devices.append(
                    {
                        "device_id": row["device_id"],
                        "status": row["status"],
                        "normalized": json.loads(row["normalized_json"]),
                        "protocols": [
                            {
                                "protocol": protocol["protocol"],
                                "success": bool(protocol["success"]),
                                "usable": bool(protocol["usable"]),
                                "error": protocol["error"],
                                "duration_ms": protocol["duration_ms"],
                                "normalized": json.loads(protocol["normalized_json"]),
                                "raw": json.loads(protocol["raw_json"]),
                            }
                            for protocol in protocol_rows
                        ],
                    }
                )
            return {
                "inspection": {
                    "id": inspection["id"],
                    "rack_id": inspection["rack_id"],
                    "status": inspection["status"],
                    "conclusion": inspection["conclusion"],
                    "started_at": inspection["started_at"],
                    "completed_at": inspection["completed_at"],
                },
                "devices": devices,
            }

    def sensor_series(
        self,
        *,
        device_id: str,
        sensor_name: str,
        start: str | None = None,
        end: str | None = None,
        limit: int = 1000,
    ) -> list[dict]:
        clauses = ["device_id=?", "sensor_name=?"]
        params: list = [device_id, sensor_name]
        if start:
            clauses.append("observed_at>=?")
            params.append(start)
        if end:
            clauses.append("observed_at<=?")
            params.append(end)
        params.append(max(1, min(int(limit), 10000)))
        rows = self._fetchall(
            "SELECT inspection_id, device_id, sensor_name, kind, value, unit, "
            "lower_bound, upper_bound, source_protocol, observed_at "
            "FROM sensor_observations WHERE "
            + " AND ".join(clauses)
            + " ORDER BY observed_at, id LIMIT ?",
            tuple(params),
        )
        return [
            {
                "inspection_id": row["inspection_id"],
                "device_id": row["device_id"],
                "sensor": row["sensor_name"],
                "kind": row["kind"],
                "value": row["value"],
                "unit": row["unit"],
                "lower": row["lower_bound"],
                "upper": row["upper_bound"],
                "source_protocol": row["source_protocol"],
                "observed_at": row["observed_at"],
            }
            for row in rows
        ]

    def metrics_snapshot(self, config: dict) -> dict:
        with self._lock:
            success = {
                row["device_id"]: row["count"]
                for row in self.connection.execute(
                    "SELECT device_id, COUNT(*) AS count FROM inspection_devices "
                    "WHERE status IN ('ok', 'degraded') GROUP BY device_id"
                )
            }
            protocol_failures = {
                (row["device_id"], row["protocol"]): row["count"]
                for row in self.connection.execute(
                    "SELECT device_id, protocol, COUNT(*) AS count FROM protocol_results "
                    "WHERE success=0 GROUP BY device_id, protocol"
                )
            }
            fallback = {
                row["device_id"]: row["count"]
                for row in self.connection.execute(
                    "SELECT device_id, COUNT(*) AS count FROM inspection_devices "
                    "WHERE fallback_used=1 GROUP BY device_id"
                )
            }
            active = {
                row["device_id"]: row["count"]
                for row in self.connection.execute(
                    "SELECT device_id, COUNT(*) AS count FROM violations "
                    "WHERE status='active' GROUP BY device_id"
                )
            }
            unparsable = {
                row["device_id"]: row["count"]
                for row in self.connection.execute(
                    "SELECT device_id, COUNT(*) AS count FROM unparsable_fields "
                    "GROUP BY device_id"
                )
            }
        protocols_by_device = {}
        for device in config["devices"]:
            profile = config["firmware_profiles"][device["firmware_profile"]]
            protocols_by_device[device["device_id"]] = list(profile["protocols"])
        return {
            "devices": [device["device_id"] for device in config["devices"]],
            "success": success,
            "protocol_failures": protocol_failures,
            "fallback": fallback,
            "active": active,
            "unparsable": unparsable,
            "protocols_by_device": protocols_by_device,
        }

    @staticmethod
    def render_prometheus(snapshot: dict) -> str:
        lines = [
            "# HELP oob_collection_success_total Successful device inspections.",
            "# TYPE oob_collection_success_total counter",
        ]
        for device_id in snapshot["devices"]:
            lines.append(
                f'oob_collection_success_total{{device="{device_id}"}} '
                f'{int(snapshot["success"].get(device_id, 0))}'
            )
        lines.extend(
            [
                "# HELP oob_protocol_failures_total Failed protocol collection attempts.",
                "# TYPE oob_protocol_failures_total counter",
            ]
        )
        for device_id in snapshot["devices"]:
            for protocol in snapshot["protocols_by_device"].get(device_id, []):
                value = int(
                    snapshot["protocol_failures"].get((device_id, protocol), 0)
                )
                lines.append(
                    f'oob_protocol_failures_total{{device="{device_id}",'
                    f'protocol="{protocol}"}} {value}'
                )
        lines.extend(
            [
                "# HELP oob_fallback_collections_total Inspections completed through a fallback protocol.",
                "# TYPE oob_fallback_collections_total counter",
            ]
        )
        for device_id in snapshot["devices"]:
            lines.append(
                f'oob_fallback_collections_total{{device="{device_id}"}} '
                f'{int(snapshot["fallback"].get(device_id, 0))}'
            )
        lines.extend(
            [
                "# HELP oob_active_violations Currently active sensor violations.",
                "# TYPE oob_active_violations gauge",
            ]
        )
        for device_id in snapshot["devices"]:
            lines.append(
                f'oob_active_violations{{device="{device_id}"}} '
                f'{int(snapshot["active"].get(device_id, 0))}'
            )
        lines.extend(
            [
                "# HELP oob_unparsable_fields_total Unparsable fields observed.",
                "# TYPE oob_unparsable_fields_total counter",
            ]
        )
        for device_id in snapshot["devices"]:
            lines.append(
                f'oob_unparsable_fields_total{{device="{device_id}"}} '
                f'{int(snapshot["unparsable"].get(device_id, 0))}'
            )
        return "\n".join(lines) + "\n"
