#!/usr/bin/env python3
"""Migrate all seeded tenants, then reconcile all 20k orders."""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
if str(ROOT / "scripts") not in sys.path:
    sys.path.insert(0, str(ROOT / "scripts"))

from orders_migration.config import Config
from run_acceptance import api, start_server, stop_server, wait_health


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default=str(ROOT / "config.local.json"))
    parser.add_argument("--fresh", action="store_true")
    parser.add_argument(
        "--report", default=str(ROOT / "artifacts" / "full-migration.json")
    )
    args = parser.parse_args()
    config_path = Path(args.config).resolve()
    config = Config.load(config_path)
    environment = dict(os.environ)
    report: dict[str, Any] = {
        "ok": False,
        "startedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    server: subprocess.Popen[str] | None = None
    try:
        if args.fresh:
            for suffix in ("", "-wal", "-shm"):
                path = Path(str(config.sqlite_path) + suffix)
                if path.exists():
                    path.unlink()
            subprocess.run(
                [
                    sys.executable,
                    str(ROOT / "scripts" / "bootstrap_local.py"),
                    "--config",
                    str(config_path),
                    "--fresh",
                ],
                cwd=ROOT,
                check=True,
                env=environment,
            )

        server = start_server(
            config_path,
            ROOT / "artifacts" / "full-migration-service.log",
            environment,
        )
        wait_health(config.host, config.port)
        status, tenant_body = api(
            config.host, config.port, "GET", "/api/v1/tenants"
        )
        if status != 200:
            raise RuntimeError(f"cannot list tenants: {status} {tenant_body}")
        tenants = [int(value) for value in tenant_body["tenants"]]
        migrations = []
        for tenant in tenants:
            status, body = api(
                config.host,
                config.port,
                "POST",
                "/api/v1/migrations",
                {"tenantId": tenant},
            )
            if status != 200 or body.get("status") != "COMPLETE":
                raise RuntimeError(
                    f"tenant {tenant} migration failed: {status} {body}"
                )
            migrations.append(
                {
                    "tenantId": tenant,
                    "shard": body["shard"],
                    "lastOrderId": body["lastOrderId"],
                }
            )

        status, check = api(
            config.host,
            config.port,
            "POST",
            "/api/v1/checks",
            {"force": True},
        )
        if status != 200:
            raise RuntimeError(f"full check failed: {status} {check}")
        report.update(
            {
                "ok": check["status"] == "COMPLETE"
                and check["mismatchCount"] == 0
                and check["totalChecked"] == 20000,
                "tenantCount": len(tenants),
                "migrations": migrations,
                "fullCheck": {
                    "runId": check["runId"],
                    "status": check["status"],
                    "totalChecked": check["totalChecked"],
                    "mismatchCount": check["mismatchCount"],
                },
            }
        )
        return 0 if report["ok"] else 1
    except Exception as exc:
        report["error"] = {"type": type(exc).__name__, "message": str(exc)}
        return 1
    finally:
        stop_server(server)
        report["completedAt"] = time.strftime(
            "%Y-%m-%dT%H:%M:%SZ", time.gmtime()
        )
        output = Path(args.report)
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(
            json.dumps(report, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        print(json.dumps(report, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    raise SystemExit(main())
