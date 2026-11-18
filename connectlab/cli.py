from __future__ import annotations

import argparse
import asyncio
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

from .config import load_config
from .util import json_pretty
from .workspace import ensure_workspace


def _api_base(config: dict[str, Any]) -> str:
    return f"http://{config['service']['host']}:{config['service']['port']}"


def _request_json(
    config: dict[str, Any],
    method: str,
    path: str,
    payload: dict[str, Any] | None = None,
) -> tuple[int, dict[str, Any]]:
    data = None
    headers = {"Accept": "application/json"}
    if payload is not None:
        data = json.dumps(payload, sort_keys=True).encode("utf-8")
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(
        _api_base(config) + path,
        data=data,
        headers=headers,
        method=method,
    )
    try:
        with urllib.request.urlopen(request, timeout=300) as response:
            return response.status, json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def main(argv: list[str] | None = None) -> int:
    raw_argv = list(sys.argv[1:] if argv is None else argv)
    config_args: list[str] = []
    remaining_args: list[str] = []
    index = 0
    while index < len(raw_argv):
        token = raw_argv[index]
        if token == "--config" and index + 1 < len(raw_argv):
            config_args.extend([token, raw_argv[index + 1]])
            index += 2
        elif token.startswith("--config="):
            config_args.append(token)
            index += 1
        else:
            remaining_args.append(token)
            index += 1

    parser = argparse.ArgumentParser(
        prog="connectlab",
        description="Local per-hop connection reset attribution service",
    )
    parser.add_argument("--config")
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("init", help="generate deterministic samples and local configs")
    sub.add_parser("serve", help="start API, observer proxy, and test upstream")

    batch = sub.add_parser("batch", help="submit a controlled batch through the API")
    batch.add_argument("--batch-id", required=True)
    batch.add_argument("--count", type=int)
    batch.add_argument("--concurrency", type=int)
    batch.add_argument("--path", action="append", dest="paths")
    batch.add_argument("--target")
    batch.add_argument("--injection-id")
    batch.add_argument("--batch-timeout-ms", type=int)
    batch.add_argument("--per-hop-timeout-ms", type=int)
    batch.add_argument("--request-timeout-ms", type=int)

    status = sub.add_parser("status", help="query a committed batch")
    status.add_argument("--batch-id", required=True)

    events = sub.add_parser("events", help="query persisted events")
    events.add_argument("--batch-id", required=True)
    events.add_argument("--request-id", action="append", dest="request_ids")

    injection = sub.add_parser("inject", help="switch the active injection")
    injection.add_argument("--id")
    injection.add_argument(
        "--type",
        required=True,
        choices=["none", "syn_drop", "upstream_disconnect", "proxy_read_timeout"],
    )
    injection.add_argument("--hop")
    injection.add_argument("--ratio", type=float, default=1.0)
    injection.add_argument("--path", default="all")
    injection.add_argument("--duration-ms", type=int, default=300_000)
    injection.add_argument("--activated-at")

    sub.add_parser("metrics", help="print Prometheus exposition text")
    sub.add_parser("latest-injection", help="print the active injection config")

    args = parser.parse_args(config_args + remaining_args)
    config, _path = load_config(args.config)
    if args.command == "init":
        result = ensure_workspace(config, rewrite_configs=False)
        print(json_pretty(result))
        return 0
    if args.command == "serve":
        from .service import serve

        asyncio.run(serve(args.config))
        return 0
    if args.command == "batch":
        payload: dict[str, Any] = {"batch_id": args.batch_id}
        for key in (
            "count",
            "concurrency",
            "target",
            "injection_id",
            "batch_timeout_ms",
            "per_hop_timeout_ms",
            "request_timeout_ms",
        ):
            value = getattr(args, key, None)
            if value is not None:
                payload[key] = value
        if args.paths:
            payload["paths"] = args.paths
        status_code, result = _request_json(config, "POST", "/v1/batches", payload)
        print(json_pretty(result))
        return 0 if status_code < 400 else 1
    if args.command == "status":
        status_code, result = _request_json(
            config,
            "GET",
            "/v1/batches/" + urllib.parse.quote(args.batch_id, safe=""),
        )
        print(json_pretty(result))
        return 0 if status_code < 400 else 1
    if args.command == "events":
        query = ""
        if args.request_ids:
            query = "?" + urllib.parse.urlencode(
                [("request_id", value) for value in args.request_ids]
            )
        status_code, result = _request_json(
            config,
            "GET",
            "/v1/batches/" + urllib.parse.quote(args.batch_id, safe="") + "/events" + query,
        )
        print(json_pretty(result))
        return 0 if status_code < 400 else 1
    if args.command == "inject":
        payload = {
            "duration_ms": args.duration_ms,
            "path": args.path,
            "ratio": args.ratio,
            "type": args.type,
        }
        if args.id:
            payload["id"] = args.id
        if args.hop:
            payload["hop"] = args.hop
        if args.activated_at:
            payload["activated_at"] = args.activated_at
        status_code, result = _request_json(config, "POST", "/v1/injections", payload)
        print(json_pretty(result))
        return 0 if status_code < 400 else 1
    if args.command == "metrics":
        url = _api_base(config) + "/metrics"
        with urllib.request.urlopen(url, timeout=30) as response:
            sys.stdout.write(response.read().decode("utf-8"))
        return 0
    if args.command == "latest-injection":
        status_code, result = _request_json(
            config, "GET", "/v1/injections/latest"
        )
        print(json_pretty(result))
        return 0 if status_code < 400 else 1
    return 2
