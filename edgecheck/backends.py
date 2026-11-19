"""Three deterministic async backends with distinct protocol behaviors."""
from __future__ import annotations

import asyncio

from .dataset import Dataset, DIAG_PATH
from .h1 import H1Server, write_response
from .h2util import H2Reset, H2Resp, run_h2_server


def _vary_headers(dataset: Dataset, sample, accept_encoding):
    """Return (headers, body, chunked) for a backend-A vary sample."""
    base = [
        ("Content-Type", "application/octet-stream"),
        ("Cache-Control", "public, max-age=300"),
        ("Vary", "Accept-Encoding"),
        ("X-EdgeCheck-Backend", "A"),
    ]
    if accept_encoding and "gzip" in accept_encoding.lower():
        return base + [("Content-Encoding", "gzip")], \
            dataset.gzip_wire(sample), False
    return base, dataset.identity_body(sample), sample.chunked


def make_backend_a(dataset: Dataset) -> H1Server:
    """HTTP/1.1 only; chunked on /a/vary/1x; rejects h2 prior-knowledge."""

    async def preface(reader, writer):
        await write_response(writer, "GET", 505,
                             [("X-EdgeCheck-Backend", "A")],
                             b"http/2 not supported here")
        return "close"

    async def handler(req, reader, writer):
        sample = dataset.by_path.get(req.target.split("?", 1)[0])
        if sample is None or sample.backend != "A":
            await write_response(writer, req.method, 404, [], b"not found")
            return None
        headers, body, chunked = _vary_headers(
            dataset, sample, req.header("accept-encoding"))
        if req.method == "HEAD":
            headers = [(k, v) for k, v in headers if k != "Content-Encoding"]
            await write_response(writer, req.method, 200, headers,
                                 dataset.identity_body(sample))
            return None
        await write_response(writer, req.method, 200, headers, body,
                             chunked=chunked,
                             trickle=0.05 if chunked else 0.0)
        return None

    return H1Server(handler, preface_handler=preface)


def _backend_b_response(dataset: Dataset, method: str, path: str,
                        headers_in) -> H2Resp:
    sample = dataset.by_path.get(path)
    base = [
        ("content-type", "application/octet-stream"),
        ("cache-control", "public, max-age=300"),
        ("x-edgecheck-backend", "B"),
    ]
    if sample is None or sample.backend != "B":
        return H2Resp(404, list(base), b"not found")
    if sample.head_mismatch and method == "HEAD":
        declared = dataset.head_declared_length(sample)
        return H2Resp(200, base + [("content-length", str(declared))], b"")
    if sample.etag:
        etag = dataset.etag_value(sample)
        inm = None
        for k, v in headers_in:
            if k.lower() == "if-none-match":
                inm = v
        if inm is not None and inm.strip() == etag:
            return H2Resp(304, base + [("etag", etag)], b"")
        body = dataset.identity_body(sample)
        return H2Resp(200, base + [("etag", etag),
                                   ("content-length", str(len(body)))], body)
    body = dataset.identity_body(sample)
    return H2Resp(200, base + [("content-length", str(len(body)))], body)


def make_backend_b(dataset: Dataset) -> H1Server:
    """HTTP/1.1 + h2c prior-knowledge; HEAD/GET Content-Length mismatch."""

    async def preface(reader, writer):
        async def h2_handler(method, path, headers):
            return _backend_b_response(dataset, method, path, headers)
        await run_h2_server(reader, writer, h2_handler,
                            preface_head=b"PRI * HTTP/2.0\r\n\r\n")
        return "hijack"

    async def handler(req, reader, writer):
        resp = _backend_b_response(dataset, req.method,
                                   req.target.split("?", 1)[0], req.headers)
        headers = [(k.title(), v) for k, v in resp.headers]
        await write_response(writer, req.method, resp.status, headers,
                             resp.body)
        return None

    return H1Server(handler, preface_handler=preface)


def make_backend_c(dataset: Dataset) -> H1Server:
    """h2c only: plain HTTP/1.1 gets 426, prior-knowledge h2 is reset,
    HTTP/1.1 Upgrade: h2c is accepted."""

    async def preface(reader, writer):
        transport = writer.transport
        if transport is not None:
            transport.abort()
        return "hijack"

    async def h2_handler(method, path, headers):
        if path == DIAG_PATH:
            raise H2Reset()
        sample = dataset.by_path.get(path)
        base = [
            ("content-type", "application/octet-stream"),
            ("cache-control", "public, max-age=300"),
            ("x-edgecheck-backend", "C"),
        ]
        if sample is None or sample.backend != "C":
            return H2Resp(404, list(base), b"not found")
        body = dataset.identity_body(sample)
        return H2Resp(200, base + [("content-length", str(len(body)))], body)

    async def handler(req, reader, writer):
        upgrade = (req.header("upgrade") or "").lower()
        settings = req.header("http2-settings")
        if upgrade == "h2c" and settings:
            writer.write(b"HTTP/1.1 101 Switching Protocols\r\n"
                         b"Connection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
            await writer.drain()
            path = req.target.split("?", 1)[0]
            await run_h2_server(
                reader, writer, h2_handler,
                upgraded_settings=settings.encode(),
                first_request_meta=(req.method, path, req.headers))
            return "hijack"
        await write_response(
            writer, req.method, 426,
            [("Upgrade", "h2c"), ("Connection", "Upgrade"),
             ("X-EdgeCheck-Backend", "C")],
            b"h2c upgrade required")
        return None

    return H1Server(handler, preface_handler=preface)
