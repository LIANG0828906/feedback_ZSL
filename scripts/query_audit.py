from __future__ import annotations

import argparse
import json
import sqlite3
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Query the append-only order-guard audit table"
    )
    parser.add_argument("database", help="SQLite database path")
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--after-seq", type=int, default=0)
    parser.add_argument("--limit", type=int, default=1000)
    args = parser.parse_args()
    connection = sqlite3.connect(Path(args.database))
    connection.row_factory = sqlite3.Row
    rows = connection.execute(
        """
        SELECT seq, run_id, event_type, tenant_id, from_state, to_state,
               business_code, reason, metrics_snapshot_json,
               thresholds_json, sli_snapshot_json, decision_json, created_at
        FROM audit_events
        WHERE run_id = ? AND seq > ?
        ORDER BY seq
        LIMIT ?
        """,
        (args.run_id, args.after_seq, args.limit),
    ).fetchall()
    connection.close()
    output = []
    for row in rows:
        item = dict(row)
        for key in (
            "metrics_snapshot_json",
            "thresholds_json",
            "sli_snapshot_json",
            "decision_json",
        ):
            item[key.removesuffix("_json")] = json.loads(item.pop(key))
        output.append(item)
    print(json.dumps(output, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
