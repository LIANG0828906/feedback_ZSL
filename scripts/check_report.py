#!/usr/bin/env python3
"""Verify scenario report invariants and cross-check them against SQLite."""

from __future__ import annotations

import argparse
import json
import sqlite3
import sys
from pathlib import Path


def fail(message: str) -> None:
    raise AssertionError(message)


def check_ingress(name: str, data: dict, expected_sessions: int) -> None:
    if data["session_count"] != expected_sessions:
        fail(f"{name}: session_count={data['session_count']} expected={expected_sessions}")
    if data["migration_count"] <= 0:
        fail(f"{name}: no migrations")
    if data["migration_ack_preserved_ratio"] != 1:
        fail(f"{name}: ack preservation ratio={data['migration_ack_preserved_ratio']}")
    if data["migration_limit_preserved_ratio"] != 1:
        fail(f"{name}: limit preservation ratio={data['migration_limit_preserved_ratio']}")
    if data["rate_limit_events"] <= 0:
        fail(f"{name}: no rate limit events")
    if not data["all_limits_have_backend_basis"]:
        fail(f"{name}: a rate limit lacks backend basis")
    if data["backend_backlog_peak"] < 5:
        fail(f"{name}: backlog peak={data['backend_backlog_peak']} did not cross threshold")
    if data["forward_processing_p99_ms"] <= 0:
        fail(f"{name}: missing forwarding P99")
    if not data["protocols"]:
        fail(f"{name}: negotiated protocol not recorded")


def check_sqlite(report: dict, db_path: Path) -> None:
    con = sqlite3.connect(db_path)
    con.row_factory = sqlite3.Row
    try:
        for session in report["sessions"]:
            row = con.execute(
                "SELECT * FROM sessions WHERE id=?", (session["session_id"],)
            ).fetchone()
            if row is None:
                fail(f"session {session['session_id']} missing from SQLite")
            if row["success_count"] != session["success_fragments"]:
                fail(f"{session['device_id']}: success mismatch report={session['success_fragments']} db={row['success_count']}")
            if row["migration_count"] != session["migration_count"]:
                fail(f"{session['device_id']}: migration count mismatch")
            if row["ack_through"] != session["final_ack_through"]:
                fail(f"{session['device_id']}: ack_through mismatch")
            if json.loads(row["ack_ranges"]) != session["accepted_ranges"]:
                fail(f"{session['device_id']}: accepted range mismatch")
            if row["backlog_peak"] != session["backend_backlog_peak"]:
                fail(f"{session['device_id']}: backlog peak mismatch")
            if row["send_window_peak"] != session["send_window_peak"]:
                fail(f"{session['device_id']}: send window peak mismatch")
            migs = con.execute(
                "SELECT COUNT(*) n, SUM(preserved_limited) preserved FROM migrations WHERE session_id=?",
                (session["session_id"],),
            ).fetchone()
            if migs["n"] != session["migration_count"]:
                fail(f"{session['device_id']}: migration rows mismatch")
            preserved_report = sum(1 for m in session.get("migrations", []) if m["preserved_limit_active"])
            if (migs["preserved"] or 0) != preserved_report:
                fail(f"{session['device_id']}: preserved limit mismatch")
            feedback = con.execute(
                "SELECT COUNT(*) n FROM feedback_events WHERE session_id=? AND source='backend_backpressure'",
                (session["session_id"],),
            ).fetchone()["n"]
            if feedback != len(session.get("rate_limit_events") or []):
                fail(f"{session['device_id']}: rate feedback event mismatch")
            rejections = con.execute(
                "SELECT COUNT(*) n, COALESCE(SUM(CASE WHEN recovered_at_unix_ns>0 THEN 1 ELSE 0 END),0) recovered "
                "FROM rejections WHERE session_id=?",
                (session["session_id"],),
            ).fetchone()
            if rejections["n"] != len(session.get("rejections") or []):
                fail(f"{session['device_id']}: rejection rows mismatch")
            if rejections["n"] != rejections["recovered"]:
                fail(f"{session['device_id']}: rejected fragment was not recovered")
    finally:
        con.close()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--report", default="artifacts/scenario-report.json")
    parser.add_argument("--db", default="artifacts/run/edgegateway.db")
    parser.add_argument("--metrics", default="artifacts/run/metrics.txt")
    args = parser.parse_args()
    report = json.loads(Path(args.report).read_text(encoding="utf-8"))
    if report.get("device_count") != 24:
        fail(f"device_count={report.get('device_count')}")
    check_ingress("quic", report["ingresses"]["quic"], 12)
    check_ingress("tcp", report["ingresses"]["tcp"], 12)
    quic_protocols = report["ingresses"]["quic"]["protocols"]
    tcp_protocols = report["ingresses"]["tcp"]["protocols"]
    if "HTTP/3.0" not in quic_protocols:
        fail(f"QUIC protocols={quic_protocols}")
    if not ({"HTTP/2.0", "HTTP/1.1"} & set(tcp_protocols)):
        fail(f"TCP protocols={tcp_protocols}")
    rejections = sum(s["rejection_responses"] for s in report["sessions"])
    if rejections < 2:
        fail(f"rejection responses={rejections}")
    if report["backend"]["duplicate_fragment_requests"] != 0:
        fail("backend accepted a duplicate fragment")
    limit = report["session_concurrency_limit"]
    if not limit["respected"] or limit["overlimit_business_error_code"] != "SESSION_LIMIT_REACHED":
        fail(f"session limit probe={limit}")
    check_sqlite(report, Path(args.db))
    metrics_text = Path(args.metrics).read_text(encoding="utf-8")
    for required in (
        "edge_gateway_active_sessions",
        "edge_gateway_fragments_total",
        "edge_gateway_forward_duration_seconds_bucket",
    ):
        if required not in metrics_text:
            fail(f"metrics endpoint missing {required}")
    print(
        json.dumps(
            {
                "ok": True,
                "migrations": report["ingresses"]["quic"]["migration_count"]
                + report["ingresses"]["tcp"]["migration_count"],
                "rate_limit_events": report["ingresses"]["quic"]["rate_limit_events"]
                + report["ingresses"]["tcp"]["rate_limit_events"],
                "rejections": rejections,
                "session_limit_code": limit["overlimit_business_error_code"],
            },
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AssertionError as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
