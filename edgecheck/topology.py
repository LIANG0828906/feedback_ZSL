"""Startup topology probes: verify each backend's protocol behavior and
the Varnish management channel. Any failure aborts with E_TOPOLOGY."""
from __future__ import annotations

import asyncio

from .config import Config
from .h1 import h1_fetch
from .h2util import H2CUpgradedClient, H2ClientConn


async def port_in_use(host: str, port: int, timeout: float = 0.5) -> bool:
    # Note: this environment silently drops SYNs to closed ports instead
    # of refusing them, so a short timeout means "not in use".
    try:
        _, writer = await asyncio.wait_for(
            asyncio.open_connection(host, port), timeout)
        writer.close()
        return True
    except (OSError, asyncio.TimeoutError):
        return False


async def _preface_probe(host: str, port: int, timeout: float = 5.0) -> str:
    """Send the h2 prior-knowledge preface; return 'rejected' or 'accepted'
    depending on how the server reacts."""
    try:
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection(host, port), timeout)
    except OSError:
        return "rejected"
    try:
        writer.write(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
        await writer.drain()
        try:
            data = await asyncio.wait_for(reader.read(4096), timeout)
        except (asyncio.TimeoutError, ConnectionResetError):
            return "rejected"
        if not data:
            return "rejected"
        first_line = data.split(b"\r\n")[0]
        if first_line.startswith(b"HTTP/1.1 505"):
            return "rejected"
        return "accepted"
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def probe_backends(config: Config) -> dict:
    bind = config.bind
    results: dict = {}

    resp = await h1_fetch(bind, config.backend_a_port, "GET", "/a/vary/00",
                          [("Accept-Encoding", "identity")])
    results["backend_a_http1"] = resp.status == 200
    results["backend_a_rejects_h2"] = (
        await _preface_probe(bind, config.backend_a_port) == "rejected")

    resp = await h1_fetch(bind, config.backend_b_port, "GET", "/b/plain/00")
    results["backend_b_http1"] = resp.status == 200
    try:
        conn = await H2ClientConn.connect(bind, config.backend_b_port)
        h2resp = await conn.request(
            "GET", "/b/plain/00", f"{bind}:{config.backend_b_port}")
        results["backend_b_h2c_prior_knowledge"] = h2resp.status == 200
        await conn.close()
    except Exception:
        results["backend_b_h2c_prior_knowledge"] = False

    resp = await h1_fetch(bind, config.backend_c_port, "GET", "/c/plain/00")
    results["backend_c_plain_http1_status"] = resp.status
    results["backend_c_h2_prior_rejected"] = (
        await _preface_probe(bind, config.backend_c_port) == "rejected")
    try:
        session, first = await H2CUpgradedClient.connect(
            bind, config.backend_c_port, "GET", "/c/plain/00",
            f"{bind}:{config.backend_c_port}")
        results["backend_c_h2c_upgrade"] = first.status == 200
        await session.close()
    except Exception:
        results["backend_c_h2c_upgrade"] = False

    return results


def topology_ok(protocols: dict, management: bool) -> bool:
    return (
        protocols.get("backend_a_http1") is True
        and protocols.get("backend_a_rejects_h2") is True
        and protocols.get("backend_b_http1") is True
        and protocols.get("backend_b_h2c_prior_knowledge") is True
        and protocols.get("backend_c_plain_http1_status") == 426
        and protocols.get("backend_c_h2_prior_rejected") is True
        and protocols.get("backend_c_h2c_upgrade") is True
        and management
    )
