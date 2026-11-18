"""Per-tenant SQLite state: committed tasks and partial aggregates.

Tenants never share state files; intermediate results are fully isolated.
"""

import os
import sqlite3

SCHEMA = """
CREATE TABLE IF NOT EXISTS tasks (
    task_id TEXT PRIMARY KEY,
    side TEXT NOT NULL,
    vehicle TEXT NOT NULL,
    unit INTEGER NOT NULL,
    rows INTEGER NOT NULL,
    attempts INTEGER NOT NULL,
    status TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS partials (
    task_id TEXT NOT NULL,
    side TEXT NOT NULL,
    vehicle TEXT NOT NULL,
    window_start INTEGER NOT NULL,
    cnt INTEGER NOT NULL,
    sum_speed INTEGER NOT NULL,
    sum_fuel INTEGER NOT NULL,
    sum_dist INTEGER NOT NULL,
    PRIMARY KEY (task_id, window_start)
);
"""


class TenantStore:
    def __init__(self, path, fresh):
        if fresh and os.path.exists(path):
            os.remove(path)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        self.conn = sqlite3.connect(path)
        self.conn.executescript(SCHEMA)
        self.conn.commit()

    def close(self):
        self.conn.close()

    def committed_task_ids(self):
        cur = self.conn.execute(
            "SELECT task_id FROM tasks WHERE status = 'COMMITTED'")
        return {row[0] for row in cur.fetchall()}

    def commit(self, task, partials, attempts):
        with self.conn:
            self.conn.execute(
                "INSERT OR REPLACE INTO tasks"
                " (task_id, side, vehicle, unit, rows, attempts, status)"
                " VALUES (?, ?, ?, ?, ?, ?, 'COMMITTED')",
                (task["task_id"], task["side"], task["vehicle"],
                 task["unit"], task["rows"], attempts))
            self.conn.executemany(
                "INSERT OR REPLACE INTO partials"
                " (task_id, side, vehicle, window_start, cnt,"
                "  sum_speed, sum_fuel, sum_dist)"
                " VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                [(task["task_id"], task["side"], task["vehicle"], ws,
                  agg[0], agg[1], agg[2], agg[3])
                 for ws, agg in partials.items()])

    def merge(self, side):
        """Merge committed partials into final per (vehicle, window) sums."""
        cur = self.conn.execute(
            "SELECT vehicle, window_start, SUM(cnt), SUM(sum_speed),"
            " SUM(sum_fuel), SUM(sum_dist) FROM partials WHERE side = ?"
            " GROUP BY vehicle, window_start", (side,))
        merged = {}
        for vehicle, ws, cnt, ss, sf, sd in cur.fetchall():
            merged[(vehicle, ws)] = (cnt, ss, sf, sd)
        return merged

    def completed_vehicles(self):
        cur = self.conn.execute(
            "SELECT DISTINCT vehicle FROM tasks"
            " WHERE status = 'COMMITTED' ORDER BY vehicle")
        return [row[0] for row in cur.fetchall()]
