"""Bridge: HTTP/1.1 (Varnish side) <-> h2c upgrade (backend C side).

Varnish 7.4 cannot speak HTTP/2 to backends, so backend C is fronted by
this lightweight in-process bridge that terminates HTTP/1.1 and forwards
over a persistent h2c-upgraded connection. Upstream h2c failures surface
as 502 + Cache-Control: no-store (never cacheable per VCL).
"""
from __future__ import annotations

import asyncio
import logging

from .h1 import H1Server, write_response
from .h2util import H2CUpgradedClient

log = logging.getLogger("edgecheck.bridge")

HOP_BY_HOP = {"connection", "keep-alive", "proxy-authenticate",
              "proxy-authorization", "te", "trailer", "transfer-encoding",
              "upgrade", "host"}


class Bridge:
    def __init__(self, c_host: str, c_port: int, authority: str,
                 timeout: float = 15.0):
        self.c_host = c_host
        self.c_port = c_port
        self.authority = authority
        self.timeout = timeout
        self._session: H2CUpgradedClient | None = None
        self._lock = asyncio.Lock()
        self.server: H1Server | None = None

    async def _request(self, path: str,
                       fwd: list[tuple[str, str]]):
        """Returns (status, headers, body) or raises."""
        async with self._lock:
            if self._session is None or not self._session.alive:
                if self._session is not None:
                    await self._session.close()
                session, first = await H2CUpgradedClient.connect(
                    self.c_host, self.c_port, "GET", path, self.authority,
                    headers=fwd, timeout=self.timeout)
                self._session = session
                return first.status, first.headers, first.body
            session = self._session
        resp = await session.request("GET", path, self.authority,
                                     headers=fwd, timeout=self.timeout)
        return resp.status, resp.headers, resp.body

    async def handle(self, req, reader, writer):
        path = req.target
        fwd = [(k, v) for k, v in req.headers
               if k.lower() not in HOP_BY_HOP]
        try:
            status, headers, body = await self._request(path, fwd)
        except Exception as exc:
            log.info("upstream h2c failure for %s: %s", path, exc)
            # session is dead; drop it so the next request reconnects
            if self._session is not None:
                await self._session.close()
                self._session = None
            await write_response(
                writer, req.method, 502,
                [("Cache-Control", "no-store"),
                 ("X-EdgeCheck-Upstream-Error", "E_UPSTREAM_PROTOCOL"),
                 ("X-EdgeCheck-Backend", "C")],
                b"upstream h2c protocol failure")
            return None
        out = [(k, v) for k, v in headers
               if k.lower() not in HOP_BY_HOP and not k.startswith(":")]
        out.append(("X-EdgeCheck-Upstream-Protocol", "h2"))
        await write_response(writer, req.method, status, out, body)
        return None

    async def start(self, host: str, port: int) -> None:
        self.server = H1Server(self.handle)
        await self.server.start(host, port)

    async def stop(self) -> None:
        if self._session is not None:
            await self._session.close()
        if self.server is not None:
            await self.server.stop()
