"""SQLite time-series storage for the collector (stdlib sqlite3 only)."""

import sqlite3
import threading

SCHEMA = """
CREATE TABLE IF NOT EXISTS samples (
    device TEXT NOT NULL,
    cycle INTEGER NOT NULL,
    oid TEXT NOT NULL,
    ts_cs INTEGER,
    value TEXT NOT NULL,
    value_type TEXT NOT NULL,
    PRIMARY KEY (device, cycle, oid)
);
CREATE INDEX IF NOT EXISTS idx_samples_device_oid_cycle
    ON samples (device, oid, cycle);
CREATE TABLE IF NOT EXISTS counter_deltas (
    device TEXT NOT NULL,
    cycle INTEGER NOT NULL,
    if_index INTEGER NOT NULL,
    direction TEXT NOT NULL,
    delta INTEGER,
    elapsed_cs INTEGER,
    status TEXT NOT NULL,
    reason TEXT,
    PRIMARY KEY (device, cycle, if_index, direction)
);
CREATE TABLE IF NOT EXISTS device_cycles (
    device TEXT NOT NULL,
    cycle INTEGER NOT NULL,
    status TEXT NOT NULL,
    reason TEXT,
    timeouts INTEGER NOT NULL DEFAULT 0,
    oid_failures INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (device, cycle)
);
CREATE TABLE IF NOT EXISTS alerts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device TEXT NOT NULL,
    if_index INTEGER NOT NULL,
    raised_cycle INTEGER NOT NULL,
    cleared_cycle INTEGER,
    cause TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS alert_events (
    device TEXT NOT NULL,
    cycle INTEGER NOT NULL,
    if_index INTEGER NOT NULL,
    event TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS collection_errors (
    device TEXT NOT NULL,
    cycle INTEGER NOT NULL,
    oid TEXT NOT NULL,
    reason TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS device_state (
    device TEXT PRIMARY KEY,
    last_cycle INTEGER NOT NULL,
    last_uptime_cs INTEGER NOT NULL
);
"""


class Storage:
    def __init__(self, path):
        self.conn = sqlite3.connect(path, check_same_thread=False)
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.conn.execute("PRAGMA synchronous=NORMAL")
        self.conn.executescript(SCHEMA)
        self.lock = threading.Lock()

    def close(self):
        with self.lock:
            self.conn.close()

    def max_cycle(self, device):
        row = self.conn.execute(
            "SELECT COALESCE(MAX(cycle), 0) FROM device_cycles WHERE device = ?",
            (device,)).fetchone()
        return int(row[0])

    def device_uptime(self, device):
        row = self.conn.execute(
            "SELECT last_cycle, last_uptime_cs FROM device_state WHERE device = ?",
            (device,)).fetchone()
        return (int(row[0]), int(row[1])) if row else None

    def _latest_by_oid(self, device, oid_prefix):
        rows = self.conn.execute(
            "SELECT oid, value, ts_cs FROM samples s "
            "WHERE device = ? AND oid LIKE ? "
            "AND cycle = (SELECT MAX(cycle) FROM samples "
            "             WHERE device = s.device AND oid = s.oid)",
            (device, oid_prefix + ".%")).fetchall()
        out = {}
        for oid, value, ts_cs in rows:
            out[int(oid.rsplit(".", 1)[1])] = (value, ts_cs)
        return out

    def latest_counters(self, device, in_prefix, out_prefix):
        result = {}
        for direction, prefix in (("in", in_prefix), ("out", out_prefix)):
            for if_index, (value, ts_cs) in self._latest_by_oid(device, prefix).items():
                result[(if_index, direction)] = (int(value), ts_cs)
        return result

    def latest_oper_status(self, device, oper_prefix):
        return {i: int(v) for i, (v, _) in
                self._latest_by_oid(device, oper_prefix).items()}

    def open_alerts(self, device):
        rows = self.conn.execute(
            "SELECT id, if_index FROM alerts WHERE device = ? AND cleared_cycle IS NULL",
            (device,)).fetchall()
        return {int(if_index): int(alert_id) for alert_id, if_index in rows}

    def last_alert(self, device, if_index):
        row = self.conn.execute(
            "SELECT id, raised_cycle, cleared_cycle FROM alerts "
            "WHERE device = ? AND if_index = ? ORDER BY id DESC LIMIT 1",
            (device, if_index)).fetchone()
        if not row:
            return None
        return {"id": int(row[0]), "raised_cycle": int(row[1]),
                "cleared_cycle": row[2] if row[2] is None else int(row[2])}

    def write_cycle(self, device, cycle, status, reason, timeouts, oid_failures,
                    samples, deltas, alert_events, errors, uptime_cs):
        """Persist one poll cycle atomically; returns {if_index: alert_id} for raises."""
        raised = {}
        with self.lock:
            with self.conn:
                self.conn.execute(
                    "INSERT OR IGNORE INTO device_cycles "
                    "(device, cycle, status, reason, timeouts, oid_failures) "
                    "VALUES (?, ?, ?, ?, ?, ?)",
                    (device, cycle, status, reason, timeouts, oid_failures))
                self.conn.executemany(
                    "INSERT OR IGNORE INTO samples "
                    "(device, cycle, oid, ts_cs, value, value_type) "
                    "VALUES (?, ?, ?, ?, ?, ?)",
                    [(device, cycle, oid, ts, value, vtype)
                     for oid, ts, value, vtype in samples])
                self.conn.executemany(
                    "INSERT OR IGNORE INTO counter_deltas "
                    "(device, cycle, if_index, direction, delta, elapsed_cs, status, reason) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                    [(device, cycle, d["if_index"], d["direction"], d["delta"],
                      d["elapsed_cs"], d["status"], d["reason"]) for d in deltas])
                self.conn.executemany(
                    "INSERT INTO collection_errors (device, cycle, oid, reason) "
                    "VALUES (?, ?, ?, ?)",
                    [(device, cycle, oid, why) for oid, why in errors])
                for event in alert_events:
                    kind, if_index, alert_id = event
                    if kind == "raise":
                        cur = self.conn.execute(
                            "INSERT INTO alerts (device, if_index, raised_cycle, cause) "
                            "VALUES (?, ?, ?, ?)",
                            (device, if_index, cycle, "ifOperStatus up->down"))
                        alert_id = cur.lastrowid
                        raised[if_index] = alert_id
                    elif kind == "clear":
                        self.conn.execute(
                            "UPDATE alerts SET cleared_cycle = ? WHERE id = ?",
                            (cycle, alert_id))
                    elif kind == "reopen":
                        self.conn.execute(
                            "UPDATE alerts SET cleared_cycle = NULL WHERE id = ?",
                            (alert_id,))
                    self.conn.execute(
                        "INSERT INTO alert_events (device, cycle, if_index, event) "
                        "VALUES (?, ?, ?, ?)",
                        (device, cycle, if_index, kind))
                if uptime_cs is not None:
                    self.conn.execute(
                        "INSERT INTO device_state (device, last_cycle, last_uptime_cs) "
                        "VALUES (?, ?, ?) "
                        "ON CONFLICT(device) DO UPDATE SET "
                        "last_cycle = excluded.last_cycle, "
                        "last_uptime_cs = excluded.last_uptime_cs",
                        (device, cycle, uptime_cs))
        return raised
