from __future__ import annotations

import argparse
import json
import signal
import sys
import threading
from concurrent import futures
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import unquote, urlparse

import grpc

from .config import ConfigError, RouterConfig, load_router_config
from .engine import InferenceEngine
from .health import HealthManager
from .metrics import Metrics
from .pb import inference_pb2, inference_pb2_grpc
from .stats import RequestStats
from .store import RequestStore


def _canonical_json(payload: Any) -> bytes:
    return (
        json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
        + "\n"
    ).encode("utf-8")


def _reply_to_pb(reply: dict[str, Any]) -> inference_pb2.InferReply:
    calls = [
        inference_pb2.CallRecord(
            layer_id=call["layer_id"],
            replica_id=call["replica_id"],
            session_id=call["session_id"],
            latency_ms=float(call["latency_ms"]),
            timed_out=bool(call["timed_out"]),
            attempt=int(call["attempt"]),
            outcome=call["outcome"],
            error_code=call.get("error_code") or "",
        )
        for call in reply.get("calls", [])
    ]
    return inference_pb2.InferReply(
        ok=bool(reply.get("ok")),
        request_id=reply.get("request_id", ""),
        session_id=reply.get("session_id", ""),
        output=reply.get("output", []),
        l1_replica_id=reply.get("l1_replica_id") or "",
        l2_replica_id=reply.get("l2_replica_id") or "",
        attempted_replicas=reply.get("attempted_replicas", []),
        calls=calls,
        e2e_ms=float(reply.get("e2e_ms", 0.0)),
        error_code=reply.get("error_code") or "",
        deduplicated=bool(reply.get("deduplicated")),
        error_message=reply.get("error_message") or "",
        retry_switches=int(reply.get("retry_switches", 0)),
    )


class RouterGrpcService(inference_pb2_grpc.RouterInferenceServicer):
    def __init__(self, engine: InferenceEngine) -> None:
        self.engine = engine

    def Infer(self, request: inference_pb2.InferRequest, context: grpc.ServicerContext):
        if len(request.features) != self.engine.input_dim:
            return inference_pb2.InferReply(
                ok=False,
                request_id=request.request_id,
                session_id=request.session_id,
                error_code="INVALID_REQUEST",
                error_message=(
                    f"features length {len(request.features)} != expected "
                    f"{self.engine.input_dim}"
                ),
            )
        try:
            reply, _ = self.engine.submit(
                request.request_id,
                request.session_id,
                list(request.features),
            )
            return _reply_to_pb(reply)
        except ValueError as exc:
            return inference_pb2.InferReply(
                ok=False,
                request_id=request.request_id,
                session_id=request.session_id,
                error_code="INVALID_REQUEST",
                error_message=str(exc),
            )
        except Exception as exc:
            return inference_pb2.InferReply(
                ok=False,
                request_id=request.request_id,
                session_id=request.session_id,
                error_code="INTERNAL_ERROR",
                error_message=str(exc),
            )


class StatusApi:
    def __init__(
        self,
        config: RouterConfig,
        health: HealthManager,
        stats: RequestStats,
        metrics: Metrics,
        store: RequestStore,
        engine: InferenceEngine,
    ) -> None:
        self.config = config
        self.health = health
        self.stats = stats
        self.metrics = metrics
        self.store = store
        self.engine = engine

    def status(self) -> dict[str, Any]:
        health_status = self.health.status()
        replicas_by_layer = self.config.replicas_by_layer()
        layers = []
        for layer in self.config.layers:
            layers.append(
                {
                    "layer_id": layer.layer_id,
                    "name": layer.name,
                    "call_timeout_ms": layer.call_timeout_ms,
                    "p95_threshold_ms": layer.p95_threshold_ms,
                    "available_replicas": self.health.available_count(layer.layer_id),
                    "replica_groups": sorted(
                        {
                            replica.replica_group
                            for replica in replicas_by_layer[layer.layer_id]
                        }
                    ),
                }
            )
        return {
            "ok": True,
            "service": "inference-router",
            "config_fingerprint": self.config.fingerprint(),
            "layers": layers,
            "replicas": health_status["replicas"],
            "routing_preference": health_status["routing_preference"],
            "last_eviction": health_status["last_eviction"],
            "database": {
                "path": self.config.service.db_path,
                "schema_version": self.store.schema_version(),
            },
        }

    def request(self, request_id: str) -> tuple[int, dict[str, Any]]:
        row = self.store.get_request(request_id)
        if row is None:
            return 404, {
                "ok": False,
                "error_code": "REQUEST_NOT_FOUND",
                "request_id": request_id,
            }
        row.pop("features", None)
        return 200, {"ok": True, "request": row}


class HttpApiHandler(BaseHTTPRequestHandler):
    api: StatusApi
    server_version = "InferenceRouterHTTP/1.0"

    def _send_json(self, status: int, payload: dict[str, Any]) -> None:
        body = _canonical_json(payload)
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_text(self, status: int, body: bytes, content_type: str) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        path = urlparse(self.path).path
        if path == "/healthz":
            self._send_json(200, {"ok": True, "status": "serving"})
            return
        if path == "/status":
            self._send_json(200, self.api.status())
            return
        if path == "/stats":
            self._send_json(200, {"ok": True, **self.api.stats.snapshot()})
            return
        if path == "/metrics":
            self._send_text(
                200,
                self.api.metrics.render(),
                "text/plain; version=0.0.4; charset=utf-8",
            )
            return
        if path.startswith("/v1/requests/"):
            request_id = unquote(path[len("/v1/requests/") :])
            status, payload = self.api.request(request_id)
            self._send_json(status, payload)
            return
        self._send_json(404, {"ok": False, "error_code": "NOT_FOUND"})

    def do_POST(self) -> None:
        if urlparse(self.path).path != "/v1/infer":
            self._send_json(404, {"ok": False, "error_code": "NOT_FOUND"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > 1024 * 1024:
                raise ValueError("invalid Content-Length")
            payload = json.loads(self.rfile.read(length).decode("utf-8"))
            request_id = payload["request_id"]
            session_id = payload["session_id"]
            features = [float(value) for value in payload["features"]]
            reply, _ = self.api.engine.submit(request_id, session_id, features)
            self._send_json(200, reply)
        except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
            self._send_json(
                400,
                {
                    "ok": False,
                    "error_code": "INVALID_REQUEST",
                    "message": str(exc),
                },
            )
        except Exception as exc:
            self._send_json(
                500,
                {
                    "ok": False,
                    "error_code": "INTERNAL_ERROR",
                    "message": str(exc),
                },
            )

    def log_message(self, _format: str, *args: Any) -> None:
        return


def _create_http_server(api: StatusApi, address: str) -> ThreadingHTTPServer:
    host, port_text = address.rsplit(":", 1)
    handler = type("BoundHttpApiHandler", (HttpApiHandler,), {"api": api})
    server = ThreadingHTTPServer((host, int(port_text)), handler)
    server.daemon_threads = True
    return server


def _create_grpc_server(engine: InferenceEngine, config: RouterConfig) -> grpc.Server:
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=config.service.thread_pool_size),
        options=[
            ("grpc.max_send_message_length", 4 * 1024 * 1024),
            ("grpc.max_receive_message_length", 4 * 1024 * 1024),
            ("grpc.so_reuseport", 0),
        ],
    )
    inference_pb2_grpc.add_RouterInferenceServicer_to_server(
        RouterGrpcService(engine), server
    )
    host, port_text = config.service.router_grpc.rsplit(":", 1)
    bound_port = server.add_insecure_port(f"{host}:{int(port_text)}")
    if bound_port == 0:
        raise RuntimeError(f"cannot bind router gRPC address {config.service.router_grpc}")
    return server


def run_router(config_path: str) -> int:
    try:
        config = load_router_config(config_path, inspect_models=True)
        store = RequestStore(config.service.db_path)
        stats = RequestStats(config.stats.window_size, config.stats.p99_threshold_ms)

        def persist_health_event(event: dict[str, Any]) -> None:
            stats.record_event(event)
            try:
                store.append_event(event)
            except Exception:
                pass

        health = HealthManager(
            list(config.shards),
            config.layer_by_id,
            config.health,
            persist_health_event,
        )
        metrics = Metrics(health, stats, [layer.layer_id for layer in config.layers])
        engine = InferenceEngine(config, store, health, stats, metrics)
        recovered = engine.recover_pending()
        health.start_probes()
        grpc_server = _create_grpc_server(engine, config)
        api = StatusApi(config, health, stats, metrics, store, engine)
        http_server = _create_http_server(api, config.service.http)
    except ConfigError as exc:
        print(json.dumps(exc.as_dict(), sort_keys=True), file=sys.stderr)
        return 2
    except Exception as exc:
        print(
            json.dumps(
                {
                    "ok": False,
                    "error_code": "ROUTER_START_FAILED",
                    "message": str(exc),
                },
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 2

    grpc_server.start()
    http_thread = threading.Thread(
        target=http_server.serve_forever,
        name="router-http",
        daemon=True,
    )
    http_thread.start()
    print(
        json.dumps(
            {
                "ok": True,
                "event": "router_started",
                "grpc": config.service.router_grpc,
                "http": config.service.http,
                "database": config.service.db_path,
                "recovered_requests": recovered,
            },
            sort_keys=True,
        ),
        flush=True,
    )
    stop_event = threading.Event()

    def stop(_signum, _frame):
        stop_event.set()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    stop_event.wait()
    http_server.shutdown()
    http_server.server_close()
    grpc_server.stop(grace=3).wait(timeout=5)
    health.close()
    store.close()
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run the two-tier inference router")
    parser.add_argument("--config", required=True)
    args = parser.parse_args(argv)
    return run_router(args.config)


if __name__ == "__main__":
    raise SystemExit(main())
