from __future__ import annotations

import argparse
import json
import logging
import os
import signal
import threading
import time
import traceback
from concurrent import futures

import grpc

from proto import training_pb2 as pb
from proto import training_pb2_grpc as pb_grpc
from train import db
from train.common import (
    expected_shard_assignments,
    load_config,
    load_manifest_for_config,
    resolve_path,
)


LOG = logging.getLogger("parameter-shard")
OK = "OK"
SHARD_NOT_OWNER = "SHARD_NOT_OWNER"
PARAMETER_VERSION_UNAVAILABLE = "PARAMETER_VERSION_UNAVAILABLE"
PARAMETER_STATE_UNAVAILABLE = "PARAMETER_STATE_UNAVAILABLE"
INVALID_ARGUMENT = "INVALID_ARGUMENT"


class ParameterShard:
    def __init__(self, config_path: str, shard_id: str):
        self.config = load_config(config_path)
        self.manifest = load_manifest_for_config(self.config)
        self.shard_id = shard_id
        shard_config = next(
            (item for item in self.config["shards"] if item["shard_id"] == shard_id),
            None,
        )
        if shard_config is None:
            raise ValueError(f"unknown shard id {shard_id!r}")
        self.shard_config = shard_config
        self.endpoint = f"{shard_config['host']}:{shard_config['port']}"
        self.declared_names = expected_shard_assignments(
            self.manifest, [item["shard_id"] for item in self.config["shards"]]
        )[shard_id]
        self.declared_name_set = set(self.declared_names)
        self.db_path = db.database_path(self.config)
        self.stop_event = threading.Event()
        coordinator = self.config["coordinator"]
        self.coordinator_endpoint = (
            f"{coordinator['host']}:{coordinator['port']}"
        )
        self.channel = grpc.insecure_channel(
            self.coordinator_endpoint,
            options=[
                ("grpc.max_send_message_length", 16 * 1024 * 1024),
                ("grpc.max_receive_message_length", 16 * 1024 * 1024),
            ],
        )
        self.coordinator = pb_grpc.CoordinatorStub(self.channel)

    def close(self) -> None:
        self.stop_event.set()
        self.channel.close()

    def _register_once(self) -> bool:
        try:
            reply = self.coordinator.RegisterShard(
                pb.RegisterShardRequest(
                    shard_id=self.shard_id,
                    endpoint=self.endpoint,
                    parameter_names=self.declared_names,
                    timestamp_ms=int(time.time() * 1000),
                ),
                timeout=5.0,
            )
            if reply.ok:
                LOG.info("registered %s with coordinator", self.shard_id)
                return True
            LOG.error("registration failed code=%s message=%s", reply.code, reply.message)
        except grpc.RpcError as exc:
            LOG.warning("coordinator unavailable during registration: %s", exc.code())
        return False

    def register_forever(self, deadline: float | None = None) -> None:
        limit = deadline or (
            time.monotonic()
            + float(self.config["runtime"]["grpc_retry_seconds"])
        )
        while not self.stop_event.is_set() and time.monotonic() < limit:
            if self._register_once():
                return
            self.stop_event.wait(0.3)
        raise TimeoutError(f"{self.shard_id} could not register with coordinator")

    def _heartbeat_loop(self) -> None:
        interval = (
            float(self.config["runtime"]["shard_heartbeat_interval_ms"]) / 1000.0
        )
        while not self.stop_event.is_set():
            try:
                reply = self.coordinator.Heartbeat(
                    pb.HeartbeatRequest(
                        identity_type="shard",
                        identity_id=self.shard_id,
                        local_step=0,
                        timestamp_ms=int(time.time() * 1000),
                    ),
                    timeout=2.0,
                )
                if not reply.ok:
                    self._register_once()
            except grpc.RpcError:
                self._register_once()
            self.stop_event.wait(max(0.05, min(interval, 0.5)))

    def start_heartbeat(self) -> None:
        thread = threading.Thread(
            target=self._heartbeat_loop,
            name=f"heartbeat-{self.shard_id}",
            daemon=True,
        )
        thread.start()

    def get_parameters(
        self, request: pb.GetParametersRequest
    ) -> pb.GetParametersReply:
        if request.shard_id and request.shard_id != self.shard_id:
            return pb.GetParametersReply(
                ok=False,
                code=SHARD_NOT_OWNER,
                message="request was delivered to a different shard",
            )
        requested = sorted(set(request.parameter_names))
        connection = db.connect(self.db_path)
        try:
            latest_row = connection.execute(
                "SELECT MAX(version) AS version FROM parameter_history"
            ).fetchone()
            latest = int(latest_row["version"] or 0)
            version = latest if int(request.version) < 0 else int(request.version)
            if version > latest:
                return pb.GetParametersReply(
                    ok=False,
                    code=PARAMETER_VERSION_UNAVAILABLE,
                    message=f"requested version {version} is ahead of {latest}",
                    version=latest,
                )
            rows = connection.execute(
                """
                SELECT o.parameter_name AS parameter_name, h.value AS value
                FROM shard_ownership AS o
                JOIN parameter_history AS h
                  ON h.parameter_name = o.parameter_name
                 AND h.version = ?
                WHERE o.shard_id = ?
                ORDER BY o.parameter_name
                """,
                (version, self.shard_id),
            ).fetchall()
            owned_values = {
                row["parameter_name"]: float(row["value"]) for row in rows
            }
            owned_names = {
                row["parameter_name"]
                for row in connection.execute(
                    "SELECT parameter_name FROM shard_ownership WHERE shard_id = ?",
                    (self.shard_id,),
                )
            }
            final_names = requested if requested else sorted(owned_names)
            missing_ownership = [
                name for name in final_names if name not in owned_names
            ]
            if missing_ownership:
                return pb.GetParametersReply(
                    ok=False,
                    code=SHARD_NOT_OWNER,
                    message=(
                        "shard no longer owns requested parameters: "
                        f"{missing_ownership[:5]}"
                    ),
                    version=version,
                )
            missing_values = [
                name for name in final_names if name not in owned_values
            ]
            if missing_values:
                return pb.GetParametersReply(
                    ok=False,
                    code=PARAMETER_STATE_UNAVAILABLE,
                    message=f"missing historical values: {missing_values[:5]}",
                    version=version,
                )
            return pb.GetParametersReply(
                ok=True,
                code=OK,
                message="parameter snapshot loaded from committed history",
                version=version,
                parameters=[
                    pb.Parameter(name=name, value=owned_values[name])
                    for name in final_names
                ],
            )
        finally:
            connection.close()

    def install_takeover(
        self, request: pb.InstallTakeoverRequest
    ) -> pb.InstallTakeoverReply:
        if request.target_shard_id != self.shard_id:
            return pb.InstallTakeoverReply(
                ok=False,
                code=INVALID_ARGUMENT,
                message="takeover target does not match this shard",
            )
        names = [parameter.name for parameter in request.parameters]
        if len(names) != len(set(names)):
            return pb.InstallTakeoverReply(
                ok=False,
                code=INVALID_ARGUMENT,
                message="takeover payload contains duplicate parameter names",
            )
        now = time.time()
        connection = db.connect(self.db_path)
        try:
            with db.transaction(connection):
                connection.executemany(
                    """
                    INSERT INTO shard_cache(
                        shard_id, parameter_name, value, version, generation, updated_at
                    ) VALUES (?, ?, ?, ?, ?, ?)
                    ON CONFLICT(shard_id, parameter_name) DO UPDATE SET
                        value = excluded.value,
                        version = excluded.version,
                        generation = excluded.generation,
                        updated_at = excluded.updated_at
                    """,
                    [
                        (
                            self.shard_id,
                            parameter.name,
                            float(parameter.value),
                            int(request.version),
                            int(request.generation),
                            now,
                        )
                        for parameter in request.parameters
                    ],
                )
            LOG.warning(
                "installed takeover from %s version=%s generation=%s count=%s",
                request.old_shard_id,
                request.version,
                request.generation,
                len(names),
            )
            return pb.InstallTakeoverReply(
                ok=True,
                code=OK,
                message="latest committed parameter replica installed",
                installed_count=len(names),
            )
        finally:
            connection.close()

    def ping(self, request: pb.PingRequest) -> pb.PingReply:
        connection = db.connect(self.db_path)
        try:
            count = connection.execute(
                "SELECT COUNT(*) AS count FROM shard_ownership WHERE shard_id = ?",
                (self.shard_id,),
            ).fetchone()["count"]
            version = connection.execute(
                "SELECT MAX(version) AS version FROM parameter_history"
            ).fetchone()["version"]
            return pb.PingReply(
                ok=True,
                shard_id=self.shard_id,
                local_param_count=int(count),
                version=int(version or 0),
            )
        finally:
            connection.close()


class ShardServicer(pb_grpc.ParameterShardServicer):
    def __init__(self, shard: ParameterShard):
        self.shard = shard

    def GetParameters(self, request, context):
        try:
            return self.shard.get_parameters(request)
        except Exception:
            LOG.error("get_parameters failed\n%s", traceback.format_exc())
            return pb.GetParametersReply(
                ok=False,
                code=PARAMETER_STATE_UNAVAILABLE,
                message="internal shard error",
            )

    def InstallTakeover(self, request, context):
        try:
            return self.shard.install_takeover(request)
        except Exception:
            LOG.error("install_takeover failed\n%s", traceback.format_exc())
            return pb.InstallTakeoverReply(
                ok=False,
                code="INTERNAL",
                message="internal shard error",
            )

    def Ping(self, request, context):
        return self.shard.ping(request)


def serve(config_path: str, shard_id: str) -> int:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    shard = ParameterShard(config_path, shard_id)
    pid_path = resolve_path("run/pids") / f"{shard_id}.pid"
    pid_path.parent.mkdir(parents=True, exist_ok=True)
    pid_path.write_text(str(os.getpid()), encoding="ascii")
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=8),
        options=[
            ("grpc.max_send_message_length", 16 * 1024 * 1024),
            ("grpc.max_receive_message_length", 16 * 1024 * 1024),
        ],
    )
    pb_grpc.add_ParameterShardServicer_to_server(ShardServicer(shard), server)
    bound = server.add_insecure_port(shard.endpoint)
    if bound == 0:
        raise RuntimeError(f"failed to bind shard at {shard.endpoint}")
    shard.register_forever()
    shard.start_heartbeat()
    server.start()
    LOG.info("%s listening at %s", shard_id, shard.endpoint)
    stopped = threading.Event()

    def stop(_signum=None, _frame=None):
        if not stopped.is_set():
            stopped.set()
            shard.stop_event.set()
            server.stop(grace=1.0).wait(timeout=3.0)

    for signal_name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, signal_name):
            try:
                signal.signal(getattr(signal, signal_name), stop)
            except (ValueError, OSError):
                pass
    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        stop()
    finally:
        shard.close()
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description="Run one parameter-server shard")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--shard-id", required=True)
    args = parser.parse_args()
    return serve(args.config, args.shard_id)


if __name__ == "__main__":
    raise SystemExit(main())
