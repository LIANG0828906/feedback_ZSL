# 检查点持久化：SQLite 原子串行写入，支持版本校验与断点续跑。
from __future__ import annotations

import json
import os
import sqlite3
import threading

_SCHEMA = (
    "CREATE TABLE IF NOT EXISTS meta ("
    " key TEXT PRIMARY KEY, value TEXT NOT NULL);"
    "CREATE TABLE IF NOT EXISTS lines ("
    " line_id TEXT PRIMARY KEY, ord INTEGER NOT NULL,"
    " win_start INTEGER NOT NULL, win_end INTEGER NOT NULL,"
    " screens TEXT NOT NULL, tradeoffs TEXT NOT NULL,"
    " state TEXT NOT NULL);"
)


class CheckpointStore:
    # 单写者检查点存储；所有写操作经同一把锁串行化。
    def __init__(self, persist_dir):
        os.makedirs(persist_dir, exist_ok=True)
        self.db_path = os.path.join(persist_dir, "checkpoint.db")
        self._conn = sqlite3.connect(self.db_path, check_same_thread=False)
        self._conn.execute("PRAGMA journal_mode=WAL")
        self._lock = threading.Lock()
        with self._lock, self._conn:
            self._conn.executescript(_SCHEMA)

    def _meta(self):
        rows = self._conn.execute("SELECT key, value FROM meta").fetchall()
        return dict(rows)

    # 开启/恢复一次运行，返回已提交台词编号集合。内容哈希、参数版本或
    # 算法版本任一不匹配时整体作废快照并全量重算，禁止部分混用。
    def begin(self, content_hash, param_version, algo_version):
        with self._lock, self._conn:
            meta = self._meta()
            expected = {"content_hash": content_hash,
                        "param_version": param_version,
                        "algo_version": algo_version}
            if meta and any(meta.get(k) != v for k, v in expected.items()):
                self._conn.execute("DELETE FROM lines")
                self._conn.execute("DELETE FROM meta")
                meta = {}
            if not meta:
                for k, v in expected.items():
                    self._conn.execute(
                        "INSERT INTO meta(key, value) VALUES(?, ?)", (k, v))
            rows = self._conn.execute(
                "SELECT line_id FROM lines WHERE state='COMMITTED'").fetchall()
            return {r[0] for r in rows}

    def inspect(self):
        # 只读查看（验证工具使用，不触发作废逻辑）。
        with self._lock:
            return self._meta(), self.load_lines()

    # 原子写入单句已提交台词；调用方须按时间轴顺序串行调用。
    def commit_line(self, record):
        with self._lock, self._conn:
            self._conn.execute(
                "INSERT OR REPLACE INTO lines"
                "(line_id, ord, win_start, win_end, screens, tradeoffs, state)"
                " VALUES(?, ?, ?, ?, ?, ?, 'COMMITTED')",
                (record["line_id"], record["ord"], record["win_start"],
                 record["win_end"],
                 json.dumps(record["screens"], ensure_ascii=False),
                 json.dumps(record["tradeoffs"], ensure_ascii=False)))

    def mark_failed(self, line_id, ord_, reason):
        with self._lock, self._conn:
            self._conn.execute(
                "INSERT OR REPLACE INTO lines"
                "(line_id, ord, win_start, win_end, screens, tradeoffs, state)"
                " VALUES(?, ?, -1, -1, '[]', ?, 'FAILED')",
                (line_id, ord_,
                 json.dumps({"reason": reason}, ensure_ascii=False)))

    def load_lines(self):
        rows = self._conn.execute(
            "SELECT line_id, ord, win_start, win_end, screens, tradeoffs,"
            " state FROM lines ORDER BY ord").fetchall()
        return [{"line_id": r[0], "ord": r[1], "win_start": r[2],
                 "win_end": r[3], "screens": json.loads(r[4]),
                 "tradeoffs": json.loads(r[5]), "state": r[6]}
                for r in rows]

    def close(self):
        with self._lock:
            self._conn.close()
