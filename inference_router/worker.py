from __future__ import annotations

import argparse
import json
import signal
import sys
import threading
import time
from concurrent import futures
from pathlib import Path

import grpc
import numpy as np
import onnxruntime as ort

from .config import ConfigError, load_worker_config
from .pb import inference_pb2, inference_pb2_grpc


def _error_code_for_exception(exc: Exception) -> str:
    if isinstance(exc, ValueError):
        return "WORKER_INVALID_INPUT"
    return "WORKER_INTERNAL_ERROR"


class WorkerService(inference_pb2_grpc.WorkerInferenceServicer):
    def __init__(self, config: dict) -> None:
        self.config = config
        self.layer_id = config["layer"]
        self.replica_group = config["replica_group"]
        self.replica_id = config["replica_id"]
        self.input_dim = config["input_dim"]
        self.output_dim = config["output_dim"]
        self._fault_lock = threading.Lock()
        self._delay_ms = int(config.get("delay_ms", 0))
        options = ort.SessionOptions()
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        options.log_severity_level = 3
        if not Path(config["model_path"]).is_file():
            raise ConfigError(
                "CONFIG_MODEL_MISSING",
                f"model file does not exist: {config['model_path']}",
                replica_id=self.replica_id,
                field_name="model_path",
            )
        try:
            self.session = ort.InferenceSession(
                config["model_path"],
                sess_options=options,
                providers=["CPUExecutionProvider"],
            )
        except Exception as exc:
            raise ConfigError(
                "CONFIG_MODEL_INVALID",
                f"cannot load model file: {exc}",
                replica_id=self.replica_id,
                field_name="model_path",
            ) from exc
        inputs = self.session.get_inputs()
        outputs = self.session.get_outputs()
        if len(inputs) != 1 or len(outputs) != 1:
            raise ConfigError(
                "CONFIG_DIMENSION_MISMATCH",
                "model must expose one input and one output",
                replica_id=self.replica_id,
                field_name="input_dim/output_dim",
            )
        if inputs[0].shape[-1] != self.input_dim:
            raise ConfigError(
                "CONFIG_DIMENSION_MISMATCH",
                f"model input dimension {inputs[0].shape[-1]} != configured {self.input_dim}",
                replica_id=self.replica_id,
                field_name="input_dim",
            )
        if outputs[0].shape[-1] != self.output_dim:
            raise ConfigError(
                "CONFIG_DIMENSION_MISMATCH",
                f"model output dimension {outputs[0].shape[-1]} != configured {self.output_dim}",
                replica_id=self.replica_id,
                field_name="output_dim",
            )
        self._input_name = inputs[0].name
        self._output_name = outputs[0].name

    def Infer(self, request: inference_pb2.WorkerInferRequest, context: grpc.ServicerContext):
        started = time.perf_counter()
        with self._fault_lock:
            delay_ms = self._delay_ms
        if delay_ms > 0:
            time.sleep(delay_ms / 1000.0)
        try:
            if len(request.features) != self.input_dim:
                raise ValueError(
                    f"features length {len(request.features)} != expected {self.input_dim}"
                )
            values = np.asarray(request.features, dtype=np.float32).reshape(1, self.input_dim)
            if not np.all(np.isfinite(values)):
                raise ValueError("features must all be finite")
            output = self.session.run(
                [self._output_name], {self._input_name: values}
            )[0]
            flat = np.asarray(output, dtype=np.float32).reshape(-1)
            if flat.shape[0] != self.output_dim:
                raise ValueError(
                    f"model output length {flat.shape[0]} != expected {self.output_dim}"
                )
            compute_ms = (time.perf_counter() - started) * 1000.0
            return inference_pb2.WorkerInferReply(
                ok=True,
                request_id=request.request_id,
                session_id=request.session_id,
                output=flat.tolist(),
                replica_id=self.replica_id,
                layer_id=self.layer_id,
                compute_ms=compute_ms,
            )
        except Exception as exc:
            return inference_pb2.WorkerInferReply(
                ok=False,
                request_id=request.request_id,
                session_id=request.session_id,
                replica_id=self.replica_id,
                layer_id=self.layer_id,
                error_code=_error_code_for_exception(exc),
                error_message=str(exc),
                compute_ms=(time.perf_counter() - started) * 1000.0,
            )

    def Health(self, request: inference_pb2.HealthRequest, context: grpc.ServicerContext):
        with self._fault_lock:
            delay_ms = self._delay_ms
        if delay_ms > 0:
            time.sleep(delay_ms / 1000.0)
        return inference_pb2.HealthReply(
            ok=True,
            replica_id=self.replica_id,
            layer_id=self.layer_id,
            replica_group=self.replica_group,
            status="ready",
        )

    def SetFault(self, request: inference_pb2.FaultRequest, context: grpc.ServicerContext):
        delay_ms = max(0, min(int(request.delay_ms), 60_000))
        with self._fault_lock:
            self._delay_ms = delay_ms
        return inference_pb2.FaultReply(ok=True, delay_ms=delay_ms)


def create_server(config: dict) -> tuple[grpc.Server, WorkerService]:
    service = WorkerService(config)
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=int(config.get("threads", 4))),
        options=[
            ("grpc.max_send_message_length", 4 * 1024 * 1024),
            ("grpc.max_receive_message_length", 4 * 1024 * 1024),
            ("grpc.so_reuseport", 0),
        ],
    )
    inference_pb2_grpc.add_WorkerInferenceServicer_to_server(service, server)
    host, port = config["listen_address"].rsplit(":", 1)
    server.add_insecure_port(f"{host}:{int(port)}")
    return server, service


def run_worker(config_path: str) -> int:
    try:
        config = load_worker_config(config_path)
        server, _ = create_server(config)
    except ConfigError as exc:
        print(json.dumps(exc.as_dict(), sort_keys=True), file=sys.stderr)
        return 2
    except Exception as exc:
        print(
            json.dumps(
                {
                    "ok": False,
                    "error_code": "WORKER_START_FAILED",
                    "message": str(exc),
                },
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 2
    server.start()
    print(
        json.dumps(
            {
                "ok": True,
                "event": "worker_started",
                "layer": config["layer"],
                "replica_id": config["replica_id"],
                "listen_address": config["listen_address"],
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
    server.stop(grace=2)
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run one ONNX replica over gRPC")
    parser.add_argument("--config", required=True)
    args = parser.parse_args(argv)
    return run_worker(args.config)


if __name__ == "__main__":
    raise SystemExit(main())
