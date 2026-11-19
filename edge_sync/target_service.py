"""Local target HTTP service that owns the authoritative byte-range log.

The target is deliberately independent of the sync service so it can verify
the "byte range submitted exactly once" requirement even after scheduler
retries and egress switches.  Every committed shard is stored once, keyed by
``(task_id, shard_seq)``; duplicate submissions are detected and reported
rather than appended again.
"""

from __future__ import annotations

import hashlib
import json
import sqlite3
import threading
import http.server
from pathlib import Path

from . import ERR_SHARD_CHECKSUM

SCHEMA = """
CREATE TABLE IF NOT EXISTS shard_commits (
    task_id     TEXT NOT NULL,
    shard_seq   INTEGER NOT NULL,
    start_byte  INTEGER NOT NULL,
    end_byte    INTEGER NOT NULL,
    length      INTEGER NOT NULL,
    checksum    TEXT NOT NULL,
    PRIMARY KEY (task_id, shard_seq)
);
CREATE TABLE IF NOT EXISTS task_manifests (
    task_id    TEXT PRIMARY KEY,
    total_shards INTEGER NOT NULL,
    total_bytes  INTEGER NOT NULL
);
"""


class TargetStore:
    def __init__(self, db_path: str):
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._connection = sqlite3.connect(db_path, check_same_thread=False)
        self._connection.execute("PRAGMA journal_mode=WAL")
        self._connection.executescript(SCHEMA)
        self._connection.commit()

    def register_task(self, task_id: str, total_shards: int, total_bytes: int) -> None:
        with self._lock:
            self._connection.execute(
                "INSERT OR IGNORE INTO task_manifests VALUES (?, ?, ?)",
                (task_id, total_shards, total_bytes),
            )
            self._connection.commit()

    def committed_range(self, task_id: str, shard_seq: int) -> dict | None:
        with self._lock:
            row = self._connection.execute(
                "SELECT start_byte, end_byte, length, checksum FROM shard_commits "
                "WHERE task_id=? AND shard_seq=?",
                (task_id, shard_seq),
            ).fetchone()
        if row is None:
            return None
        return {"start_byte": row[0], "end_byte": row[1], "length": row[2], "checksum": row[3]}

    def commit(self, record: dict) -> tuple[dict, bool]:
        """Commit a shard; returns (response, duplicate).

        A duplicate must carry the same checksum, otherwise it is rejected
        with a stable business error code.
        """
        key = (record["task_id"], record["shard_seq"])
        with self._lock:
            existing = self._connection.execute(
                "SELECT start_byte, end_byte, length, checksum FROM shard_commits "
                "WHERE task_id=? AND shard_seq=?",
                key,
            ).fetchone()
            if existing is not None:
                if existing[3] != record["checksum"]:
                    return {"error": ERR_SHARD_CHECKSUM, "existing_checksum": existing[3]}, False
                response = {
                    "task_id": record["task_id"],
                    "shard_seq": record["shard_seq"],
                    "start_byte": existing[0],
                    "end_byte": existing[1],
                    "length": existing[2],
                    "checksum": existing[3],
                    "duplicate": True,
                }
                return response, True
            self._connection.execute(
                "INSERT INTO shard_commits VALUES (?, ?, ?, ?, ?, ?)",
                (
                    record["task_id"],
                    record["shard_seq"],
                    record["start_byte"],
                    record["end_byte"],
                    record["length"],
                    record["checksum"],
                ),
            )
            self._connection.commit()
        return {
            "task_id": record["task_id"],
            "shard_seq": record["shard_seq"],
            "start_byte": record["start_byte"],
            "end_byte": record["end_byte"],
            "length": record["length"],
            "checksum": record["checksum"],
            "duplicate": False,
        }, False

    def task_counts(self) -> dict[str, int]:
        with self._lock:
            rows = self._connection.execute(
                "SELECT task_id, COUNT(*) FROM shard_commits GROUP BY task_id"
            ).fetchall()
        return {row[0]: row[1] for row in rows}

    def close(self) -> None:
        with self._lock:
            self._connection.close()


def _read_chunked(handler) -> bytes:
    digest = hashlib.sha256()
    total = 0
    while True:
        size_line = handler.rfile.readline().strip()
        chunk_size = int(size_line.split(b";", 1)[0], 16)
        if chunk_size == 0:
            handler.rfile.readline()
            break
        payload = handler.rfile.read(chunk_size)
        digest.update(payload)
        total += len(payload)
        handler.rfile.read(2)
    return digest.digest().hex(), total


class TargetHandler(http.server.BaseHTTPRequestHandler):
    server_version = "EdgeSyncTarget/1.0"
    protocol_version = "HTTP/1.1"

    def _json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload, sort_keys=True).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:
        if self.path != "/ingest":
            self._json(404, {"error": "NOT_FOUND"})
            return
        is_chunked = self.headers.get("Transfer-Encoding", "").lower() == "chunked"
        if is_chunked:
            record = {
                "task_id": self.headers.get("X-Task-Id"),
                "shard_seq": int(self.headers.get("X-Shard-Seq", "-1")),
                "start_byte": int(self.headers.get("X-Start-Byte", "-1")),
                "end_byte": int(self.headers.get("X-End-Byte", "-1")),
                "checksum": self.headers.get("X-Checksum"),
            }
            checksum, body_length = _read_chunked(self)
            if record.get("checksum") and record["checksum"] != checksum:
                self._json(409, {"error": ERR_SHARD_CHECKSUM, "expected": record["checksum"], "actual": checksum})
                return
            record["checksum"] = checksum
            record["length"] = body_length
        else:
            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length) if length else b"{}"
            try:
                record = json.loads(raw.decode("utf-8"))
            except json.JSONDecodeError:
                self._json(400, {"error": "BAD_JSON"})
                return
        required = ("task_id", "shard_seq", "start_byte", "end_byte", "length", "checksum")
        if any(key not in record for key in required):
            self._json(400, {"error": "MISSING_FIELDS"})
            return
        response, duplicate = self.server.store.commit(record)
        if "error" in response:
            self._json(409, response)
        else:
            self._json(200, response)

    def do_GET(self) -> None:
        if self.path == "/health":
            self._json(200, {"status": "ok", "commits": self.server.store.task_counts()})
            return
        if self.path.startswith("/range?"):
            query = self.path.split("?", 1)[1]
            params = dict(pair.split("=", 1) for pair in query.split("&") if "=" in pair)
            found = self.server.store.committed_range(params.get("task_id", ""), int(params.get("shard_seq", -1)))
            self._json(200, {"committed": found is not None, "range": found})
            return
        self._json(404, {"error": "NOT_FOUND"})

    def log_message(self, *args) -> None:
        return


class ThreadingTargetServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, store: TargetStore):
        super().__init__(address, TargetHandler)
        self.store = store


def start_target(host: str, port: int, db_path: str) -> tuple[ThreadingTargetServer, threading.Thread, TargetStore]:
    store = TargetStore(db_path)
    server = ThreadingTargetServer((host, port), store)
    thread = threading.Thread(target=server.serve_forever, name="target-http", daemon=True)
    thread.start()
    return server, thread, store
