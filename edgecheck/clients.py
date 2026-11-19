"""Dual-protocol front-door clients (HTTP/1.1 and HTTP/2) + record shaping."""
from __future__ import annotations

import gzip
import hashlib

from .config import Config
from .h1 import RawResponse, h1_fetch
from .h2util import H2ClientConn

PROTO_H1 = "http/1.1"
PROTO_H2 = "http/2"
PROTOS = (PROTO_H1, PROTO_H2)


class FrontClients:
    def __init__(self, config: Config):
        self.config = config
        self._h2: H2ClientConn | None = None

    async def start(self) -> None:
        self._h2 = await H2ClientConn.connect(
            self.config.bind, self.config.front_port,
            timeout=self.config.request_timeout)

    async def fetch(self, proto: str, method: str, path: str,
                    headers: list[tuple[str, str]] | None = None) -> RawResponse:
        if proto == PROTO_H1:
            return await h1_fetch(
                self.config.bind, self.config.front_port, method, path,
                headers=headers, timeout=self.config.request_timeout)
        assert self._h2 is not None
        return await self._h2.request(
            method, path, self.config.front_authority, headers=headers,
            timeout=self.config.request_timeout)

    async def close(self) -> None:
        if self._h2 is not None:
            await self._h2.close()


def decode_body(resp: RawResponse) -> bytes:
    if (resp.header("content-encoding") or "").lower() == "gzip":
        return gzip.decompress(resp.body)
    return resp.body


def is_cache_hit(resp: RawResponse) -> bool:
    return (resp.header("x-cache") or "").strip().upper() == "HIT" \
        and resp.status == 200


def make_record(resp: RawResponse, phase: str, rnd: int) -> dict:
    cache_hit = is_cache_hit(resp)
    decoded = decode_body(resp)
    cl = resp.header("content-length")
    return {
        "age": {
            "header_present": resp.header("age") is not None,
            "semantics": "fresh_hit" if cache_hit else "origin",
        },
        "body_length": len(decoded),
        "body_sha256": hashlib.sha256(decoded).hexdigest(),
        "cache_hit": cache_hit,
        "content_length": int(cl) if cl is not None else None,
        "http_version": resp.http_version,
        "phase": phase,
        "round": rnd,
        "status": resp.status,
        "transfer_encoding": resp.header("transfer-encoding"),
    }
