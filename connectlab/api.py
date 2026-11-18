from __future__ import annotations

import asyncio
import json
import logging
from http import HTTPStatus
from typing import Any
from urllib.parse import parse_qs, unquote, urlsplit

from .attribution import (
    FAULT_PROXY_READ_TIMEOUT,
    FAULT_SYN_NOT_REACHED_UPSTREAM,
    FAULT_UPSTREAM_EARLY_DISCONNECT,
)
from .db import (
    BatchNotFoundError,
    Database,
    DuplicateBatchError,
)
from .http import encode_response, read_request_head
from .injection import InjectionController, InjectionValidationError
from .runner import BatchBusyError, BatchValidationError, Runner
from .util import utc_now


LOGGER = logging.getLogger("connectlab.api")
MAX_API_BODY = 1024 * 1024


class ApiServer:
    def __init__(
        self,
        config: dict[str, Any],
        database: Database,
        injections: InjectionController,
        runner: Runner,
    ) -> None:
        self.config = config
        self.database = database
        self.injections = injections
        self.runner = runner
        self._server: asyncio.AbstractServer | None = None

    async def start(self) -> None:
        cfg = self.config["service"]
        self._server = await asyncio.start_server(
            self.handle_client,
            cfg["host"],
            int(cfg["port"]),
            backlog=128,
        )
        LOGGER.info("API listening on %s:%s", cfg["host"], cfg["port"])

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()

    async def handle_client(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        try:
            request = await read_request_head(reader, 10_000)
            if request is None:
                return
            method, target, headers = request
            parsed = urlsplit(target)
            path = unquote(parsed.path)
            query = parse_qs(parsed.query)
            body = b""
            length = int(headers.get("content-length", "0") or 0)
            if length:
                if length > MAX_API_BODY:
                    await self._write_json(
                        writer,
                        413,
                        {
                            "code": "BODY_TOO_LARGE",
                            "message": f"JSON body must not exceed {MAX_API_BODY} bytes",
                        },
                    )
                    return
                body = await reader.readexactly(length)

            if method == "GET" and path == "/healthz":
                await self._write_json(
                    writer,
                    200,
                    {
                        "status": "ok",
                        "time": utc_now(),
                        "version": "1.0.0",
                    },
                )
                return
            if method == "GET" and path == "/metrics":
                await self._write_metrics(writer)
                return
            if method == "POST" and path == "/v1/batches":
                await self._create_batch(writer, body)
                return
            if method == "GET" and path == "/v1/batches":
                limit = int(query.get("limit", ["100"])[0])
                await self._write_json(
                    writer,
                    200,
                    {
                        "batches": self.database.list_batches(
                            max(1, min(limit, 500))
                        )
                    },
                )
                return
            if method == "GET" and path.startswith("/v1/batches/"):
                remain = path[len("/v1/batches/") :]
                if remain.endswith("/events"):
                    batch_id = remain[: -len("/events")].strip("/")
                    await self._batch_events(writer, batch_id, query)
                elif remain.endswith("/status"):
                    batch_id = remain[: -len("/status")].strip("/")
                    await self._batch_status(writer, batch_id)
                else:
                    batch_id = remain.strip("/")
                    await self._batch_status(writer, batch_id)
                return
            if method == "POST" and path == "/v1/injections":
                await self._set_injection(writer, body)
                return
            if method == "GET" and path == "/v1/injections/latest":
                await self._write_json(
                    writer,
                    200,
                    {"injection": self.injections.current()},
                )
                return
            if method == "GET" and path == "/v1/compare":
                await self._compare(writer, query)
                return
            await self._write_json(
                writer,
                404,
                {
                    "code": "NOT_FOUND",
                    "message": f"no route for {method} {path}",
                },
            )
        except (DuplicateBatchError, BatchBusyError, BatchValidationError,
                InjectionValidationError, BatchNotFoundError) as exc:
            await self._write_json(
                writer,
                getattr(exc, "http_status", 400),
                {
                    "code": getattr(exc, "code", "BAD_REQUEST"),
                    "message": str(exc) or getattr(exc, "message", "bad request"),
                },
            )
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            await self._write_json(
                writer,
                400,
                {"code": "INVALID_JSON", "message": str(exc)},
            )
        except (ConnectionError, asyncio.IncompleteReadError):
            pass
        except Exception:
            LOGGER.exception("API request failed")
            try:
                await self._write_json(
                    writer,
                    500,
                    {
                        "code": "INTERNAL_ERROR",
                        "message": "internal server error",
                    },
                )
            except Exception:
                pass
        finally:
            try:
                writer.close()
                await writer.wait_closed()
            except (ConnectionError, OSError):
                pass

    async def _create_batch(
        self, writer: asyncio.StreamWriter, body: bytes
    ) -> None:
        payload = json.loads(body.decode("utf-8")) if body else {}
        result = await self.runner.run_batch(payload)
        await self._write_json(writer, 201, result)

    async def _set_injection(
        self, writer: asyncio.StreamWriter, body: bytes
    ) -> None:
        payload = json.loads(body.decode("utf-8")) if body else {}
        profile = await self.injections.replace(payload)
        await self._write_json(
            writer,
            201,
            {
                "injection": profile,
                "status": "active",
            },
        )

    async def _batch_status(
        self, writer: asyncio.StreamWriter, batch_id: str
    ) -> None:
        batch = self.database.get_batch(batch_id)
        requests = self.database.get_requests(batch_id)
        all_events = self.database.get_events(batch_id)
        unexplained = []
        for request in requests:
            sequence = select_request_events(all_events, request)
            request["event_count"] = len(sequence)
            request["event_sequence"] = sequence
            if not (
                request["outcome"] == "reset"
                and request["attribution_type"] == "unexplained_reset"
            ):
                continue
            unexplained.append(
                {
                    "coverage": request["coverage"],
                    "events": sequence,
                    "missing_hops": request["missing_hops"],
                    "request_id": request["request_id"],
                }
            )
        response = {
            "attribution_summary": batch["summary"],
            "batch": {
                "batch_id": batch["batch_id"],
                "completed_count": batch["completed_count"],
                "finished_at": batch["finished_at"],
                "request_count": batch["request_count"],
                "started_at": batch["started_at"],
                "status": batch["status"],
                "target": batch["target"],
                "target_profile": batch["target_profile"],
                "unfinished_count": batch["unfinished_count"],
            },
            "batch_injection": batch["injection"],
            "batch_config": batch["batch_config"],
            "latest_injection": self.injections.current(),
            "requests": requests,
            "unexplained_reset_sequences": unexplained,
        }
        if batch.get("error"):
            response["error"] = batch["error"]
        await self._write_json(writer, 200, response)

    async def _batch_events(
        self,
        writer: asyncio.StreamWriter,
        batch_id: str,
        query: dict[str, list[str]],
    ) -> None:
        if not self.database.batch_exists(batch_id):
            raise BatchNotFoundError()
        request_ids = query.get("request_id")
        selected = set(request_ids) if request_ids else None
        events = self.database.get_events(batch_id, selected)
        await self._write_json(
            writer,
            200,
            {
                "batch_id": batch_id,
                "events": events,
            },
        )

    async def _compare(
        self, writer: asyncio.StreamWriter, query: dict[str, list[str]]
    ) -> None:
        batch_a = (query.get("batch_a") or [""])[0]
        batch_b = (query.get("batch_b") or [""])[0]
        if not batch_a or not batch_b:
            raise BatchValidationError("batch_a and batch_b are required")
        first = self.database.get_batch(batch_a)
        second = self.database.get_batch(batch_b)
        await self._write_json(
            writer,
            200,
            {
                "batch_a": {
                    "batch_id": batch_a,
                    "injection": first["injection"],
                    "summary": first["summary"],
                },
                "batch_b": {
                    "batch_id": batch_b,
                    "injection": second["injection"],
                    "summary": second["summary"],
                },
                "delta": {
                    "reset_rate": round(
                        second["summary"]["reset_rate"]
                        - first["summary"]["reset_rate"],
                        6,
                    ),
                    "reuse_failures": second["summary"]["reuse_failures"]
                    - first["summary"]["reuse_failures"],
                    "timeouts": second["summary"]["timeouts"]
                    - first["summary"]["timeouts"],
                },
            },
        )

    async def _write_metrics(self, writer: asyncio.StreamWriter) -> None:
        lines = [
            "# HELP connectlab_batch_requests_total Requests in a committed batch by path.",
            "# TYPE connectlab_batch_requests_total gauge",
        ]
        batches = []
        for item in self.database.list_batches(500):
            batches.append(self.database.get_batch(item["batch_id"]))
        batches.sort(key=lambda item: item["batch_id"])

        for batch in batches:
            batch_id = _label(batch["batch_id"])
            summary = batch["summary"]
            for path, path_stats in sorted(summary.get("by_path", {}).items()):
                lines.append(
                    "connectlab_batch_requests_total"
                    f'{{batch_id="{batch_id}",path="{_label(path)}"}} '
                    f'{path_stats.get("requests", 0)}'
                )
            lines.append(
                "connectlab_batch_requests_total"
                f'{{batch_id="{batch_id}",path="all"}} '
                f'{summary.get("requests", 0)}'
            )

        lines.extend(
            [
                "# HELP connectlab_batch_attributions_total Reset/failure attribution count for a committed batch.",
                "# TYPE connectlab_batch_attributions_total gauge",
            ]
        )
        for batch in batches:
            batch_id = _label(batch["batch_id"])
            counts = batch["summary"].get("attribution_by_fault", {})
            for fault, hop in (
                (FAULT_SYN_NOT_REACHED_UPSTREAM, "proxy"),
                (FAULT_UPSTREAM_EARLY_DISCONNECT, "upstream"),
                (FAULT_PROXY_READ_TIMEOUT, "proxy"),
            ):
                lines.append(
                    "connectlab_batch_attributions_total"
                    f'{{batch_id="{batch_id}",fault_type="{fault}",hop="{hop}"}} '
                    f'{counts.get(fault, 0)}'
                )

        metric_specs = (
            (
                "connectlab_batch_unexplained_resets_total",
                "Unexplained reset count for a committed batch.",
                "unexplained_resets",
            ),
            (
                "connectlab_batch_timeouts_total",
                "Timeout count for a committed batch.",
                "timeouts",
            ),
            (
                "connectlab_batch_connection_reuses_total",
                "Proxy upstream pooled-connection reuse attempts for a committed batch.",
                "reuse_attempts",
            ),
            (
                "connectlab_batch_reuse_failures_total",
                "Requests that failed on a reused stale upstream connection.",
                "reuse_failures",
            ),
        )
        for metric, help_text, key in metric_specs:
            lines.append(f"# HELP {metric} {help_text}")
            lines.append(f"# TYPE {metric} gauge")
            for batch in batches:
                lines.append(
                    f'{metric}{{batch_id="{_label(batch["batch_id"])}"}} '
                    f'{batch["summary"].get(key, 0)}'
                )
        payload = ("\n".join(lines) + "\n").encode("utf-8")
        writer.write(
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: text/plain; version=0.0.4; charset=utf-8\r\n"
            + f"Content-Length: {len(payload)}\r\n".encode("ascii")
            + b"Connection: close\r\n\r\n"
            + payload
        )
        await writer.drain()

    async def _write_json(
        self,
        writer: asyncio.StreamWriter,
        status: int,
        payload: dict[str, Any],
    ) -> None:
        body = json.dumps(
            payload,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        try:
            reason = HTTPStatus(status).phrase
        except ValueError:
            reason = "Response"
        head = (
            f"HTTP/1.1 {status} {reason}\r\n"
            "Content-Type: application/json; charset=utf-8\r\n"
            f"Content-Length: {len(body)}\r\n"
            "Connection: close\r\n"
            "\r\n"
        ).encode("latin-1")
        writer.write(head + body)
        await writer.drain()


def select_request_events(
    all_events: list[dict[str, Any]], request: dict[str, Any]
) -> list[dict[str, Any]]:
    connection_ids = {
        value
        for value in (
            request.get("client_connection_id"),
            request.get("proxy_downstream_connection_id"),
            request.get("proxy_upstream_connection_id"),
            request.get("upstream_connection_id"),
        )
        if value
    }
    selected: dict[int, dict[str, Any]] = {}
    changed = True
    while changed:
        changed = False
        for event in all_events:
            include = event["request_id"] == request["request_id"]
            if not include and connection_ids:
                include = (
                    event["connection_id"] in connection_ids
                    or event.get("peer_connection_id") in connection_ids
                    or event["correlation_id"] in connection_ids
                )
            if include and event["event_id"] not in selected:
                selected[event["event_id"]] = event
                connection_ids.add(event["connection_id"])
                if event.get("peer_connection_id"):
                    connection_ids.add(event["peer_connection_id"])
                connection_ids.add(event["correlation_id"])
                changed = True
    return sorted(
        selected.values(),
        key=lambda event: (event["ts_mono_ms"], event["event_id"]),
    )


def _label(value: str) -> str:
    return str(value).replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')
