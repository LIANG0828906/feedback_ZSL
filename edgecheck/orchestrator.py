"""Run the full topology: backends + bridge + Varnish + dual-protocol checks."""
from __future__ import annotations

import asyncio
import hashlib
import logging
from pathlib import Path

from .backends import make_backend_a, make_backend_b, make_backend_c
from .bridge import Bridge
from .checks import CheckRunner
from .clients import FrontClients
from .config import Config
from .dataset import Dataset
from .report import (build_report, cache_key_section, dump_report,
                     topology_failure_report)
from .topology import port_in_use, probe_backends, topology_ok
from .varnish import TopologyError, VarnishInstance
from .vcl import render_vcl

log = logging.getLogger("edgecheck")


def _write_report(config: Config, report: dict) -> None:
    path = Path(config.report_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(dump_report(report))


async def _fail_topology(config: Config, detail: str,
                         protocols: dict | None = None) -> int:
    log.error("E_TOPOLOGY: %s", detail)
    _write_report(config, topology_failure_report(config, detail, protocols))
    return 2


async def async_run(config: Config) -> int:
    # 1. ports must be free
    for name, port in sorted(config.all_ports().items()):
        if await port_in_use(config.bind, port):
            return await _fail_topology(
                config, f"port {port} ({name}) already in use on {config.bind}")

    dataset = Dataset(config.seed)
    backend_a = make_backend_a(dataset)
    backend_b = make_backend_b(dataset)
    backend_c = make_backend_c(dataset)
    bridge = Bridge(config.bind, config.backend_c_port,
                    f"{config.bind}:{config.backend_c_port}",
                    timeout=config.request_timeout)
    varnish = VarnishInstance(config)
    clients = FrontClients(config)
    protocols: dict = {}

    try:
        await backend_a.start(config.bind, config.backend_a_port)
        await backend_b.start(config.bind, config.backend_b_port)
        await backend_c.start(config.bind, config.backend_c_port)
        await bridge.start(config.bind, config.bridge_port)

        # 2. backend protocol self-checks
        protocols = await probe_backends(config)
        if not topology_ok(protocols, management=True):
            return await _fail_topology(
                config, "backend protocol self-check failed", protocols)

        # 3. VCL + Varnish
        vcl_text = render_vcl(config)
        vcl_sha = hashlib.sha256(vcl_text.encode()).hexdigest()
        vcl_path = Path(config.vcl_out)
        vcl_path.parent.mkdir(parents=True, exist_ok=True)
        vcl_path.write_text(vcl_text)
        try:
            varnish.resolve()
        except TopologyError as exc:
            return await _fail_topology(config, exc.detail, protocols)
        varnish.start(str(vcl_path.resolve()))
        try:
            await varnish.wait_ready()
        except TopologyError as exc:
            return await _fail_topology(config, exc.detail, protocols)
        try:
            varnish.load_vcl("edgecheck", str(vcl_path.resolve()))
        except TopologyError as exc:
            return await _fail_topology(config, exc.detail, protocols)
        management = varnish.admin_ok("ping")
        if not management:
            return await _fail_topology(
                config, "varnish management port unreachable", protocols)
        topology = {
            "management_reachable": True,
            "protocols": protocols,
        }

        # 4. dual-protocol check phases
        await clients.start()
        runner = CheckRunner(config, dataset, clients)
        main = await runner.run_main()
        vary = await runner.run_vary()
        head = await runner.run_head()
        revalidate = await runner.run_revalidate()
        chunked = await runner.run_chunked()
        upstream = await runner.run_upstream()

        varnish_info = {
            "active_vcl": varnish.active_vcl(),
            "front_port": config.front_port,
            "management_port": config.admin_port,
            "mode": varnish.mode,
            "vcl_sha256": vcl_sha,
            "version": varnish.version,
        }
        report = build_report(
            config, dataset,
            topology=topology, varnish=varnish_info,
            cache_key=cache_key_section(), main=main, vary=vary, head=head,
            revalidate=revalidate, chunked=chunked, upstream=upstream,
            errors=runner.errors)
        _write_report(config, report)
        log.info("report written to %s (ok=%s)", config.report_path,
                 report["ok"])
        return 0 if report["ok"] else 1
    finally:
        await clients.close()
        varnish.stop()
        await bridge.stop()
        await backend_a.stop()
        await backend_b.stop()
        await backend_c.stop()
