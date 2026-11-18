from __future__ import annotations

import hashlib
import json
import random
from pathlib import Path
from typing import Any

from .config import normalize_config, resolve_path
from .util import atomic_write_json


ENVOY_CONFIG = """# Envoy 1.30 forward-proxy lab configuration.
# The observer proxy is the default local verification path because it records
# handshake, pool reuse, and per-hop close initiator events that stock Envoy
# access logs do not expose.
static_resources:
  listeners:
  - name: forward_proxy_18080
    address:
      socket_address:
        address: 127.0.0.1
        port_value: 18080
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: forward_proxy
          codec_type: AUTO
          access_log:
          - name: envoy.access_loggers.file
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.access_loggers.file.v3.FileAccessLog
              path: logs/envoy-access.log
              log_format:
                json_format:
                  start_time: "%START_TIME%"
                  method: "%REQ(:METHOD)%"
                  path: "%REQ(:PATH)%"
                  request_id: "%REQ(X-REQUEST-ID)%"
                  client_connection_id: "%REQ(X-CLIENT-CONN-ID)%"
                  downstream_remote: "%DOWNSTREAM_REMOTE_ADDRESS%"
                  upstream_host: "%UPSTREAM_HOST%"
                  response_code: "%RESPONSE_CODE%"
                  response_flags: "%RESPONSE_FLAGS%"
                  duration_ms: "%DURATION%"
          route_config:
            name: local_route
            virtual_hosts:
            - name: local_upstream
              domains: ["*"]
              routes:
              - match:
                  prefix: "/"
                route:
                  cluster: local_upstream
                  timeout: 3s
                  retry_policy:
                    retry_on: 5xx
                    num_retries: 0
          http_filters:
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
  - name: local_upstream
    type: STATIC
    connect_timeout: 0.4s
    load_assignment:
      cluster_name: local_upstream
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address:
                address: 127.0.0.1
                port_value: 18081
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http_protocol_options: {}
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: 18082
"""


def ensure_workspace(config: dict[str, Any], *, rewrite_configs: bool = False) -> dict[str, Any]:
    root = resolve_path(".")
    seed = int(config["service"]["seed"])
    data_dir = resolve_path(str(config["samples"]["dir"]))
    config_dir = root / "config"
    for path in (data_dir, config_dir, root / "var", root / "logs"):
        path.mkdir(parents=True, exist_ok=True)

    sizes = {
        "short": int(config["samples"]["short_bytes"]),
        "keepalive": int(config["samples"]["keepalive_bytes"]),
        "upload": int(config["samples"]["upload_bytes"]),
    }
    body_files: dict[str, str] = {}
    digests: dict[str, str] = {}
    for path_name, size in sizes.items():
        body_path = data_dir / f"{path_name}_body_{seed}.bin"
        if not body_path.exists() or body_path.stat().st_size != size:
            rng = random.Random(seed + sum(ord(char) for char in path_name))
            payload = bytearray(size)
            offset = 0
            while offset < size:
                chunk = rng.randbytes(min(65536, size - offset))
                payload[offset : offset + len(chunk)] = chunk
                offset += len(chunk)
            tmp = body_path.with_suffix(".tmp")
            with open(tmp, "wb") as handle:
                handle.write(payload)
                handle.flush()
            tmp.replace(body_path)
        body_files[path_name] = str(body_path.relative_to(root))
        digests[path_name] = hashlib.sha256(body_path.read_bytes()).hexdigest()

    keepalive_count = int(config["batch"]["keepalive_requests_per_connection"])
    sequence: list[str] = []
    while len(sequence) < max(24, keepalive_count * 3):
        sequence.extend(["keepalive"] * keepalive_count)
        sequence.extend(["short", "upload"])
    request_sequence = {
        "body_files": body_files,
        "body_sha256": digests,
        "keepalive_request_sequence": [
            {"ordinal": index + 1, "path": "keepalive", "request_slot": index}
            for index in range(keepalive_count)
        ],
        "paths": sequence,
        "seed": seed,
    }
    atomic_write_json(
        resolve_path(str(config["samples"]["sequence_file"])),
        request_sequence,
    )

    proxy_config = {
        "connect_timeout_ms": config["proxy"]["connect_timeout_ms"],
        "downstream_idle_timeout_ms": config["proxy"]["downstream_idle_timeout_ms"],
        "envoy_access_log": "logs/envoy-access.log",
        "envoy_admin_port": config["proxy"]["envoy_admin_port"],
        "envoy_config": config["proxy"]["envoy_config"],
        "envoy_version": "1.30",
        "listen_host": config["proxy"]["listen_host"],
        "listen_port": config["proxy"]["listen_port"],
        "mode": config["proxy"]["mode"],
        "pool_size": config["proxy"]["pool_size"],
        "targets": config.get("targets", {}),
        "upstream_read_timeout_ms": config["proxy"]["upstream_read_timeout_ms"],
    }
    upstream_config = {
        "early_disconnect_after_response_ms": config["upstream"][
            "early_disconnect_after_response_ms"
        ],
        "early_disconnect_before_response_ms": config["upstream"][
            "early_disconnect_before_response_ms"
        ],
        "extra_ports": config["upstream"].get("extra_ports", []),
        "injection_file": config["injection"]["file"],
        "listen_host": config["upstream"]["listen_host"],
        "listen_port": config["upstream"]["listen_port"],
        "proxy_read_timeout_extra_ms": config["upstream"][
            "proxy_read_timeout_extra_ms"
        ],
        "read_timeout_ms": config["upstream"]["read_timeout_ms"],
        "targets": config.get("targets", {}),
    }
    if rewrite_configs or not (config_dir / "connectlab.json").exists():
        atomic_write_json(
            config_dir / "connectlab.json",
            normalize_config(config),
        )
    if rewrite_configs or not (config_dir / "proxy.json").exists():
        atomic_write_json(config_dir / "proxy.json", proxy_config)
    if rewrite_configs or not (config_dir / "upstream.json").exists():
        atomic_write_json(config_dir / "upstream.json", upstream_config)
    envoy_path = resolve_path(str(config["proxy"]["envoy_config"]))
    if rewrite_configs or not envoy_path.exists():
        envoy_path.write_text(ENVOY_CONFIG, encoding="utf-8")
    injection_path = resolve_path(str(config["injection"]["file"]))
    if not injection_path.exists():
        from .injection import TYPE_NONE, normalize_injection

        atomic_write_json(
            injection_path,
            normalize_injection(
                {
                    "duration_ms": 31_536_000_000,
                    "id": config["injection"]["default_id"],
                    "path": "all",
                    "ratio": 0.0,
                    "type": TYPE_NONE,
                }
            ),
        )
    return {
        "body_files": body_files,
        "config_dir": str(config_dir),
        "data_dir": str(data_dir),
        "sequence_file": str(resolve_path(str(config["samples"]["sequence_file"]))),
    }
