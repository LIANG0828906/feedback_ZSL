# edgecheck — dual-protocol edge-cache compatibility checker

Validates that a Varnish Cache 7.4 edge node serves **HTTP/1.1 and HTTP/2
clients from one shared cache** with equivalent cacheability outcomes:
same cache key, same Vary bucketing, same 304/Age semantics, and
byte-identical response bodies for the same URL on both protocols.

## Topology

```
client (HTTP/1.1) --+--> Varnish 7.4 (127.0.0.1:27380, admin 27385)
client (HTTP/2 h2c) -+       |
                             +--> backend A  :27381  HTTP/1.1 only, 10 chunked responses
                             +--> backend B  :27382  HTTP/1.1 + h2c prior-knowledge,
                             |                       10 URLs HEAD Content-Length != GET length
                             +--> bridge      :27384  HTTP/1.1 -> h2c upgrade converter
                                      +--> backend C :27383  h2c only (426 without Upgrade)
```

Varnish 7.4 (community) cannot speak HTTP/2 to backends, so backend C is
fronted by a lightweight in-process bridge that terminates HTTP/1.1 for
Varnish and forwards over a persistent h2c-upgraded connection.

All three backends and the bridge are asyncio services in this repo; the
60-URL dataset (~6 MB) is generated deterministically from a fixed seed
(default 273). No external data, accounts, or network access are needed
at run time.

## Quick start

Requirements: Python 3.11+, `httpx 0.27`, `h2 4.x`, and a Varnish 7.4
`varnishd` (this workspace carries one under `.runtime/varnish`, extracted
from the official `varnishcache/varnish74` packages; override with
`EDGECHECK_VARNISHD=/path/to/varnishd`).

```sh
python3 -m edgecheck run            # full check, writes artifacts/compatibility-report.json
```

Exit codes: `0` all checks pass, `1` check failures (see report),
`2` `E_TOPOLOGY` (port busy, backend protocol self-check failed, varnishd
missing/unreachable — the run refuses to continue, never degrades silently).

Ports are configurable via CLI flags (`--front-port`, `--backend-a-port`,
...) or `EDGECHECK_*` env vars; the run first verifies every port is free.

## Cache key strategy: `host-url-vary-v1`

The cache key contains **only** `req.url` + `req.http.host` (declared
explicitly in `vcl_hash`). The client protocol version is deliberately
**not** hashed: HTTP/1.1 and HTTP/2 clients share one cache object per
URL, so neither protocol can evict the other's entries. Protocol-specific
transfer framing (chunked vs DATA frames) is a delivery-layer concern and
never enters the cache key; the report proves sharing with
`cross_protocol_shared_hits = 60` and identical per-URL content hashes on
both protocols.

Supporting VCL behavior:

- `HEAD` and conditional requests (`If-None-Match`) are `pass`ed to the
  origin: backend B's deliberately wrong HEAD `Content-Length` can never
  pollute GET cache objects, and a 304 is a genuine end-to-end
  revalidation, never a new cache fill.
- `Accept-Encoding` is canonicalized to `gzip` or *unset* before hashing,
  so `Vary: Accept-Encoding` produces exactly two buckets shared by both
  protocols.
- Only clean `200` responses are cached; error responses (including the
  bridge's `502` on upstream h2c failure) and `no-store` are marked
  uncacheable.

### Why `-p http_gzip_support=off`

Stock Varnish **ignores `Vary: Accept-Encoding`** (see `vry_cmp` in
`cache_vary.c`: "we do not vary on Accept-Encoding ... varnish will
gunzip as necessary") and forces `Accept-Encoding: gzip` onto every
backend request (`cache_fetch.c`). That collapses the two encoding
buckets into one and makes the origin's `Vary` meaningless. The service
therefore starts varnishd with `http_gzip_support=off`, which restores
honest RFC vary semantics end to end; the checker then verifies the two
buckets hold distinct content and identical cross-protocol hit behavior.

## Checks performed

- **main**: both protocols GET all 60 URLs twice (fill + hit rounds);
  records status, body length, content SHA-256, Age semantics, hit/miss.
- **vary**: 20 `Vary: Accept-Encoding` URLs requested with `identity` and
  `gzip`; buckets must be distinct, consistent, and shared.
- **head**: 10 backend-B URLs; HEAD declares GET+4096 bytes. Each
  mismatch is recorded as `E_HEAD_MISMATCH`; the follow-up GET must still
  hit the unpolluted cached object.
- **revalidate**: 5 ETag URLs; `If-None-Match` must yield empty-body 304
  (else `E_REVALIDATE`) and must not count as a cache fill.
- **chunked**: 10 backend-A chunked URLs; cache-busted origin fetch must
  show `Transfer-Encoding: chunked` on HTTP/1.1, all bodies must match
  the generator hash, and no `Transfer-Encoding` header may leak into
  HTTP/2 responses.
- **upstream**: 2 forced h2c failures against backend C
  (`/c/diag/force-reset` resets the upgraded connection); the bridge
  surfaces `502` + `E_UPSTREAM_PROTOCOL`, and Varnish must not cache it.

## Report

`artifacts/compatibility-report.json` — deterministic: sorted keys, no
absolute paths, no timestamps; identical configuration and seed produce a
byte-identical file across runs (only wall-clock duration differs).
Sections: `cache_key` (strategy + hash inputs), `config`, `dataset`,
`topology` (protocol self-checks), `varnish` (version, active VCL name,
VCL SHA-256), `urls` (per-URL dual-protocol records), `vary_checks`,
`head_checks`, `revalidate_checks`, `chunked_checks`,
`upstream_protocol_checks`, `error_counts`, `summary` (hit rates per
protocol, shared hits, expected vs unexpected error counts), `ok`.

## Resource envelope (parallel-run constraints)

- CPU: the runner pins itself and all children to at most 2 logical CPUs
  (`sched_setaffinity`); Varnish runs with `thread_pools=1`.
- Memory: Varnish store is `malloc,64m`; the whole topology stays far
  below 1.25 GiB.
- Disk: runtime state under `.runtime/` (Varnish distro ~30 MB, workdir,
  generated VCL), well below 2 GB.

## Layout

- `edgecheck/dataset.py` — seeded deterministic 60-URL dataset
- `edgecheck/backends.py` — backends A/B/C protocol behaviors
- `edgecheck/bridge.py` — HTTP/1.1 → h2c upgrade bridge for backend C
- `edgecheck/h1.py`, `edgecheck/h2util.py` — raw HTTP/1.1 and h2/h2c
  clients/servers (h2 4.x; raw HTTP/1.1 keeps transfer-encoding visible,
  which is why the checker does not rely on a high-level HTTP client)
- `edgecheck/vcl.py` — generated VCL (cache key strategy)
- `edgecheck/varnish.py` — varnishd discovery/lifecycle/admin CLI
- `edgecheck/checks.py` — check phases; `edgecheck/report.py` — report
- `edgecheck/topology.py` — startup protocol probes (`E_TOPOLOGY`)
