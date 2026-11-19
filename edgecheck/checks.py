"""Check phases: main dual-protocol passes, Vary, HEAD, 304, chunked, h2c."""
from __future__ import annotations

import hashlib

from .clients import FrontClients, PROTOS, is_cache_hit, make_record
from .config import Config
from .dataset import DIAG_PATH, Dataset, Sample

ERROR_CODES = ("E_HEAD_MISMATCH", "E_REVALIDATE", "E_UPSTREAM_PROTOCOL")


class CheckRunner:
    def __init__(self, config: Config, dataset: Dataset,
                 clients: FrontClients):
        self.config = config
        self.dataset = dataset
        self.clients = clients
        self.errors = {code: 0 for code in ERROR_CODES}

    def expected_hash(self, sample: Sample) -> str:
        return hashlib.sha256(
            self.dataset.identity_body(sample)).hexdigest()

    async def run_main(self) -> dict[str, dict]:
        out: dict[str, dict] = {}
        for proto in PROTOS:
            for rnd in (1, 2):
                for sample in self.dataset.samples:
                    resp = await self.clients.fetch(
                        proto, "GET", sample.path,
                        [("Accept-Encoding", "identity")])
                    rec = make_record(resp, "main", rnd)
                    slot = out.setdefault(sample.path,
                                          {p: [] for p in PROTOS})
                    slot[proto].append(rec)
                    if sample.backend == "C":
                        slot.setdefault("_upstream_proto", set()).add(
                            resp.header("x-edgecheck-upstream-protocol"))
        return out

    async def run_vary(self) -> list[dict]:
        results = []
        for sample in self.dataset.select(vary=True):
            buckets = {}
            for bucket in ("identity", "gzip"):
                if bucket == "identity":
                    content = self.dataset.identity_body(sample)
                else:
                    content = self.dataset.gzip_variant(sample)
                content_sha = hashlib.sha256(content).hexdigest()
                entry = {
                    "body_length": len(content),
                    "content_sha256": content_sha,
                    "vary_header_ok": True,
                }
                ok = True
                for proto in PROTOS:
                    recs = []
                    for rnd in (1, 2):
                        resp = await self.clients.fetch(
                            proto, "GET", sample.path,
                            [("Accept-Encoding", bucket)])
                        if resp.header("vary") != "Accept-Encoding":
                            entry["vary_header_ok"] = False
                        rec = make_record(resp, "vary", rnd)
                        if rec["status"] != 200 or \
                                rec["body_sha256"] != content_sha or \
                                rec["body_length"] != len(content):
                            ok = False
                        recs.append(rec)
                    entry[proto] = recs
                entry["shared_cache_hit"] = entry[PROTOS[1]][0]["cache_hit"]
                entry["ok"] = ok
                buckets[bucket] = entry
            distinct = buckets["identity"]["content_sha256"] != \
                buckets["gzip"]["content_sha256"]
            ok = distinct and all(b["ok"] and b["vary_header_ok"]
                                  for b in buckets.values())
            results.append({
                "bucket_hashes_distinct": distinct,
                "buckets": buckets,
                "ok": ok,
                "sample_id": sample.sample_id,
                "url": sample.path,
            })
        return results

    async def run_head(self) -> list[dict]:
        results = []
        for sample in self.dataset.select(head_mismatch=True):
            expected_sha = self.expected_hash(sample)
            entry: dict = {
                "sample_id": sample.sample_id,
                "url": sample.path,
            }
            any_mismatch = False
            no_pollution = True
            for proto in PROTOS:
                head_resp = await self.clients.fetch(proto, "HEAD",
                                                     sample.path)
                get_resp = await self.clients.fetch(
                    proto, "GET", sample.path,
                    [("Accept-Encoding", "identity")])
                declared = head_resp.header("content-length")
                declared = int(declared) if declared is not None else None
                get_len = len(get_resp.body)
                get_sha = hashlib.sha256(get_resp.body).hexdigest()
                get_hit = is_cache_hit(get_resp)
                get_ok = get_resp.status == 200 and get_len == sample.size \
                    and get_sha == expected_sha
                mismatch = declared != get_len
                any_mismatch = any_mismatch or mismatch
                no_pollution = no_pollution and get_hit and get_ok
                entry[proto] = {
                    "backend_declared_content_length":
                        self.dataset.head_declared_length(sample),
                    "declared_content_length": declared,
                    "get_body_length": get_len,
                    "get_cache_hit": get_hit,
                    "get_matches_expected": get_ok,
                    "head_body_length": len(head_resp.body),
                    "head_status": head_resp.status,
                    "mismatch": mismatch,
                }
            entry["error_code"] = "E_HEAD_MISMATCH" if any_mismatch else None
            entry["head_does_not_pollute_get"] = no_pollution
            if any_mismatch:
                self.errors["E_HEAD_MISMATCH"] += 1
            results.append(entry)
        return results

    async def run_revalidate(self) -> list[dict]:
        results = []
        for sample in self.dataset.select(etag=True):
            etag = self.dataset.etag_value(sample)
            expected_sha = self.expected_hash(sample)
            entry: dict = {
                "sample_id": sample.sample_id,
                "url": sample.path,
            }
            entry_ok = True
            for proto in PROTOS:
                cond_resp = await self.clients.fetch(
                    proto, "GET", sample.path, [("If-None-Match", etag)])
                cond_rec = make_record(cond_resp, "revalidate", 1)
                post_resp = await self.clients.fetch(
                    proto, "GET", sample.path,
                    [("Accept-Encoding", "identity")])
                post_rec = make_record(post_resp, "post_revalidate_get", 1)
                conditional_ok = cond_resp.status == 304 \
                    and len(cond_resp.body) == 0
                post_get_ok = post_rec["status"] == 200 \
                    and post_rec["body_sha256"] == expected_sha \
                    and post_rec["body_length"] == sample.size \
                    and post_rec["cache_hit"]
                counts_as_fill = not post_rec["cache_hit"]
                entry[proto] = {
                    "conditional": cond_rec,
                    "conditional_ok": conditional_ok,
                    "counts_as_cache_fill": counts_as_fill,
                    "post_get": post_rec,
                    "post_get_ok": post_get_ok,
                }
                entry_ok = entry_ok and conditional_ok and post_get_ok \
                    and not counts_as_fill
            entry["error_code"] = None if entry_ok else "E_REVALIDATE"
            if not entry_ok:
                self.errors["E_REVALIDATE"] += 1
            results.append(entry)
        return results

    async def run_chunked(self) -> list[dict]:
        results = []
        for sample in self.dataset.select(chunked=True):
            expected_sha = self.expected_hash(sample)
            entry: dict = {
                "backend": "A",
                "expected_body_length": sample.size,
                "expected_body_sha256": expected_sha,
                "sample_id": sample.sample_id,
                "url": sample.path,
            }
            ok = True
            h2_te_present = False
            for proto in PROTOS:
                recs = []
                slug = "http1" if proto == PROTOS[0] else "http2"
                bust = f"{sample.path}?edgecheck-chunked={slug}"
                for target in (bust, sample.path):
                    resp = await self.clients.fetch(
                        proto, "GET", target,
                        [("Accept-Encoding", "identity")])
                    te = resp.header("transfer-encoding")
                    body_sha = hashlib.sha256(resp.body).hexdigest()
                    rec = {
                        "body_length": len(resp.body),
                        "body_sha256": body_sha,
                        "status": resp.status,
                        "transfer_encoding": te,
                    }
                    if resp.status != 200 or body_sha != expected_sha \
                            or len(resp.body) != sample.size:
                        ok = False
                    if proto == PROTOS[1] and te is not None:
                        h2_te_present = True
                    recs.append(rec)
                if proto == PROTOS[0] and \
                        recs[0]["transfer_encoding"] != "chunked":
                    ok = False  # origin chunked path was not exercised
                entry[proto] = recs
            entry["http2_transfer_encoding_present"] = h2_te_present
            entry["ok"] = ok and not h2_te_present
            entry["error_code"] = None if entry["ok"] else "E_CHUNKED"
            results.append(entry)
        return results

    async def run_upstream(self) -> list[dict]:
        events = []
        for attempt, proto in ((1, PROTOS[0]), (2, PROTOS[1])):
            resp = await self.clients.fetch(proto, "GET", DIAG_PATH)
            upstream_err = resp.header("x-edgecheck-upstream-error")
            cache_hit = is_cache_hit(resp)
            ok = resp.status == 502 \
                and upstream_err == "E_UPSTREAM_PROTOCOL" \
                and not cache_hit
            events.append({
                "attempt": attempt,
                "backend": "C",
                "cache_hit": cache_hit,
                "error_code": "E_UPSTREAM_PROTOCOL",
                "forced": True,
                "ok": ok,
                "status": resp.status,
                "url": DIAG_PATH,
            })
            self.errors["E_UPSTREAM_PROTOCOL"] += 1
        return [{
            "events": events,
            "sample_id": "C-DIAG",
            "url": DIAG_PATH,
        }]
