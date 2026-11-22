"""Independent result verification from persisted artefacts only.

The verifier rebuilds nothing beyond re-running queries against the on-disk
index, then compares against either:

* a previously saved baseline result file (exact field comparison), or
* the manifest ground truth (family physical identity + score floor).

The first discrepancy is reported with query id, match source and field.
"""

from __future__ import annotations

from dataclasses import dataclass

from .config import Config
from .errors import SchemaError
from .hashing import read_json
from .indexstore import INDEX_FORMAT_VERSION, IndexStore
from .manifest import Manifest, load_manifest
from .search import SearchResult, Searcher

# Fields compared exactly; timestamps use a numeric tolerance.
EXACT_FIELDS = ("file_id", "physical_key", "format_kind", "container")
NUMERIC_FIELDS = ("start_s", "end_s", "confidence")
NUMERIC_TOL = 1e-4


@dataclass
class VerificationIssue:
    query_id: str
    match_source: str
    field: str
    expected: object
    actual: object

    def to_dict(self) -> dict:
        return vars(self)


@dataclass
class VerificationReport:
    ok: bool
    checked_queries: int
    issues: list[VerificationIssue]

    def to_dict(self) -> dict:
        return {
            "ok": self.ok,
            "checked_queries": self.checked_queries,
            "first_issue": self.issues[0].to_dict() if self.issues else None,
            "issues": [i.to_dict() for i in self.issues],
        }


def run_queries_from_persistence(manifest_path: str, index_dir: str,
                                 cfg: Config, *,
                                 format_version: str = INDEX_FORMAT_VERSION
                                 ) -> list[SearchResult]:
    """Re-run every manifest query using only persisted inputs."""
    manifest = load_manifest(manifest_path)
    store = IndexStore(index_dir).load(
        cfg, manifest_fingerprint=manifest.fingerprint,
        expected_format=format_version,
    )
    searcher = Searcher(store, cfg, manifest)
    results: list[SearchResult] = []
    for qid, query in sorted(manifest.queries_meta.items()):
        # Prefer the lossy version to exercise cross-format querying.
        owned = [m for m in manifest.query_files()
                 if m.file_id in (f["file_id"] for f in query["files"])]
        media = next((m for m in owned if m.format_kind == "lossy"), owned[0])
        results.append(searcher.query(media, query_id=qid))
    return results


def verify_against_baseline(manifest_path: str, index_dir: str,
                            baseline_path: str, cfg: Config
                            ) -> VerificationReport:
    try:
        baseline = read_json(baseline_path)
    except ValueError as exc:
        raise SchemaError("baseline result file is not valid JSON",
                          details={"path": baseline_path,
                                   "reason": str(exc)}) from exc
    if not isinstance(baseline, list):
        raise SchemaError("baseline must be a list of query results")

    actual = run_queries_from_persistence(manifest_path, index_dir, cfg)
    issues: list[VerificationIssue] = []
    by_id = {r.query_id: r for r in actual}
    for expected in baseline:
        qid = expected.get("query_id", "<unknown>")
        if qid not in by_id:
            issues.append(VerificationIssue(
                qid, "<query>", "query_id", qid, "missing in rerun"))
            continue
        result = by_id[qid]
        issues.extend(_compare_query(qid, expected, result.to_dict()))
    return VerificationReport(not issues, len(actual), issues)


def verify_against_ground_truth(manifest_path: str, index_dir: str,
                                cfg: Config) -> VerificationReport:
    manifest = load_manifest(manifest_path)
    results = run_queries_from_persistence(manifest_path, index_dir, cfg)
    issues: list[VerificationIssue] = []
    for result in results:
        query = manifest.queries_meta[result.query_id]
        # The query phrase is embedded inside the group's long master, so the
        # expected family identity is the group, and its location must cover
        # the declared target segment window.
        expected_group = query["group_id"]
        target_segment = query["target_segment"]
        group = manifest.groups[expected_group]
        segment_truth = next(
            s for s in group["segments"]
            if s["segment_id"] == target_segment
        )
        if not result.families:
            issues.append(VerificationIssue(
                result.query_id, "<family>", "families",
                expected_group, "no family returned"))
            continue
        top = result.families[0]
        # Family merge correctness: every member shares one physical key.
        physicals = {m.physical_key for m in top.members}
        if physicals != {group["physical_key"]}:
            issues.append(VerificationIssue(
                result.query_id, top.physical_key, "physical_key",
                group["physical_key"], sorted(physicals)))
        if top.group_id != expected_group:
            issues.append(VerificationIssue(
                result.query_id, top.physical_key, "group_id",
                expected_group, top.group_id))
        # Located window must overlap the ground-truth segment window.
        if not (top.end_s >= segment_truth["start_s"]
                and top.start_s <= segment_truth["end_s"]):
            issues.append(VerificationIssue(
                result.query_id, top.physical_key, "segment_window",
                [segment_truth["start_s"], segment_truth["end_s"]],
                [top.start_s, top.end_s]))
        # Cross-format coverage: pcm + lossy members both present.
        kinds = {m.format_kind for m in top.members}
        if kinds != {"pcm", "lossy"}:
            issues.append(VerificationIssue(
                result.query_id, top.physical_key, "format_coverage",
                ["pcm", "lossy"], sorted(kinds)))
        # No physical segment may be counted twice across families.
        seen: dict[str, str] = {}
        for fam in result.families:
            for member in fam.members:
                if member.file_id in seen and seen[member.file_id] != fam.physical_key:
                    issues.append(VerificationIssue(
                        result.query_id, member.file_id, "duplicate_count",
                        fam.physical_key, seen[member.file_id]))
                seen[member.file_id] = fam.physical_key
        # Confidence ordering and validity.
        scores = [f.confidence for f in result.families]
        if scores != sorted(scores, reverse=True):
            issues.append(VerificationIssue(
                result.query_id, top.physical_key, "family_order",
                "descending confidence", scores))
        for member in top.members:
            if not 0.0 <= member.confidence <= 1.0:
                issues.append(VerificationIssue(
                    result.query_id, member.file_id, "confidence",
                    "[0,1]", member.confidence))
            if member.end_s < member.start_s:
                issues.append(VerificationIssue(
                    result.query_id, member.file_id, "time_range",
                    "end>=start", [member.start_s, member.end_s]))
    return VerificationReport(not issues, len(results), issues)


def _compare_query(qid: str, expected: dict, actual: dict
                   ) -> list[VerificationIssue]:
    issues: list[VerificationIssue] = []
    exp_matches = expected.get("matches", [])
    act_matches = actual.get("matches", [])
    if len(exp_matches) != len(act_matches):
        issues.append(VerificationIssue(
            qid, "<matches>", "match_count",
            len(exp_matches), len(act_matches)))
        return issues
    for exp, act in zip(exp_matches, act_matches):
        source = act.get("file_id", "<unknown>")
        for field in EXACT_FIELDS:
            if exp.get(field) != act.get(field):
                issues.append(VerificationIssue(
                    qid, source, field, exp.get(field), act.get(field)))
        for field in NUMERIC_FIELDS:
            ev, av = exp.get(field), act.get(field)
            if ev is None or av is None or abs(float(ev) - float(av)) > NUMERIC_TOL:
                issues.append(VerificationIssue(
                    qid, source, field, ev, av))
    exp_families = expected.get("families", [])
    act_families = actual.get("families", [])
    if [f["physical_key"] for f in exp_families] != [
        f["physical_key"] for f in act_families
    ]:
        issues.append(VerificationIssue(
            qid, "<families>", "physical_key_order",
            [f["physical_key"] for f in exp_families],
            [f["physical_key"] for f in act_families]))
    return issues
