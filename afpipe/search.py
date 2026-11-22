"""Similar-segment search, alignment scoring and family merging."""

from __future__ import annotations

from collections import defaultdict
from dataclasses import dataclass, field

from .config import Config
from .errors import BudgetError, ShortQueryError
from .fingerprint import (
    HAMMING_THRESHOLD,
    PREFIX_BITS,
    FingerprintTrack,
    FrameFingerprint,
    code_bit_count,
    code_value,
    coarse_key,
    hamming_similarity,
)
from .indexstore import IndexEntry, IndexStore
from .manifest import Manifest, MediaFile
from .pipeline import process_file


@dataclass
class Match:
    entry_id: str
    file_id: str
    source_path: str
    format_kind: str
    container: str
    physical_key: str
    group_id: str
    start_s: float
    end_s: float
    query_start_s: float
    query_end_s: float
    aligned_frames: int
    confidence: float

    def to_dict(self) -> dict:
        return vars(self)


@dataclass
class Family:
    physical_key: str
    group_id: str
    confidence: float
    start_s: float
    end_s: float
    members: list[Match] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "physical_key": self.physical_key,
            "group_id": self.group_id,
            "confidence": round(self.confidence, 6),
            "start_s": round(self.start_s, 6),
            "end_s": round(self.end_s, 6),
            "members": [m.to_dict() for m in self.members],
            "version_scores": [
                {"file_id": m.file_id, "format_kind": m.format_kind,
                 "container": m.container, "confidence": round(m.confidence, 6)}
                for m in self.members
            ],
        }


@dataclass
class SearchResult:
    query_id: str
    query_file_id: str
    duration_s: float
    matches: list[Match]
    families: list[Family]
    postings_scanned: int

    def to_dict(self) -> dict:
        return {
            "query_id": self.query_id,
            "query_file_id": self.query_file_id,
            "duration_s": round(self.duration_s, 6),
            "postings_scanned": self.postings_scanned,
            "matches": [m.to_dict() for m in self.matches],
            "families": [f.to_dict() for f in self.families],
        }


def _align(query: FingerprintTrack, postings: dict[str, list[list]],
           cfg: Config, scan_budget: list[int]) -> tuple[list[dict], int]:
    """Coarse-prefix candidates -> Hamming verification -> diagonal voting."""
    info_bits = code_bit_count(cfg.quant_bits, cfg.bands)
    prefix_bits = PREFIX_BITS[cfg.quant_bits]
    threshold = HAMMING_THRESHOLD[cfg.quant_bits]
    diagonals: dict[int, dict] = defaultdict(
        lambda: {"hits": 0, "q_idx": [], "e_idx": [], "e_time": []}
    )
    scanned = 0
    for frame in query.frames:
        value = code_value(frame)
        key = str(coarse_key(value, info_bits, prefix_bits))
        hits = postings.get(key)
        if not hits:
            continue
        for entry_idx, entry_time, entry_value in hits:
            offset = frame.index - entry_idx
            similarity = hamming_similarity(value, entry_value, info_bits)
            scanned += 1
            scan_budget[0] += 1
            if scan_budget[0] > cfg.max_postings_scan:
                raise BudgetError(
                    "query exceeded postings scan budget",
                    details={"budget": cfg.max_postings_scan},
                )
            if similarity < 1.0 - threshold:
                continue
            diag = diagonals[offset]
            diag["hits"] += 1
            diag["q_idx"].append(frame.index)
            diag["e_idx"].append(entry_idx)
            diag["e_time"].append(entry_time)
    candidates = []
    for offset, diag in diagonals.items():
        if diag["hits"] < cfg.min_aligned_frames:
            continue
        candidates.append({
            "offset": offset,
            "hits": diag["hits"],
            "q_min": min(diag["q_idx"]),
            "q_max": max(diag["q_idx"]),
            "e_min": min(diag["e_idx"]),
            "e_max": max(diag["e_idx"]),
            "t_min": min(diag["e_time"]),
            "t_max": max(diag["e_time"]),
        })
    return candidates, scanned


class Searcher:
    def __init__(self, store: IndexStore, cfg: Config, manifest: Manifest):
        self.store = store
        self.cfg = cfg
        self.manifest = manifest

    def query(self, media: MediaFile, *, query_id: str) -> SearchResult:
        duration_ms = media.duration_s * 1000.0
        if duration_ms < self.cfg.min_query_ms:
            raise ShortQueryError(
                "query shorter than minimum searchable length",
                details={"file_id": media.file_id,
                         "duration_ms": round(duration_ms, 3),
                         "min_query_ms": self.cfg.min_query_ms},
            )
        if duration_ms > self.cfg.max_query_ms:
            raise BudgetError(
                "query longer than configured resource cap",
                details={"file_id": media.file_id,
                         "duration_ms": round(duration_ms, 3),
                         "max_query_ms": self.cfg.max_query_ms},
            )

        processed = process_file(media, self.manifest, self.cfg)
        qtrack: FingerprintTrack = processed.track
        if len(qtrack.frames) < self.cfg.min_aligned_frames:
            raise ShortQueryError(
                "query yields too few frames",
                details={"frames": len(qtrack.frames),
                         "min_aligned_frames": self.cfg.min_aligned_frames},
            )

        scan_budget = [0]
        matches: list[Match] = []
        total_scanned = 0
        hop_s = self.cfg.hop_samples() / self.cfg.sample_rate
        for entry in self.store.entries:
            if entry.role != "index":
                continue
            postings = self.store.postings(entry.entry_id)
            candidates, scanned = _align(
                qtrack, postings, self.cfg, scan_budget
            )
            total_scanned += scanned
            for cand in candidates:
                confidence = cand["hits"] / len(qtrack.frames)
                if confidence < self.cfg.family_score_floor:
                    continue
                start_s = max(0.0, cand["t_min"])
                end_s = min(entry.duration_s,
                            cand["t_max"] + self.cfg.frame_ms / 1000.0)
                matches.append(Match(
                    entry_id=entry.entry_id,
                    file_id=entry.file_id,
                    source_path=entry.path,
                    format_kind=entry.format_kind,
                    container=entry.container,
                    physical_key=entry.physical_key,
                    group_id=entry.group_id,
                    start_s=round(start_s, 6),
                    end_s=round(end_s, 6),
                    query_start_s=round(cand["q_min"] * hop_s, 6),
                    query_end_s=round(
                        cand["q_max"] * hop_s + self.cfg.frame_ms / 1000.0, 6),
                    aligned_frames=cand["hits"],
                    confidence=round(confidence, 6),
                ))

        matches.sort(key=lambda m: (-m.confidence, m.physical_key, m.file_id))
        families = self._merge_families(matches)
        return SearchResult(
            query_id=query_id,
            query_file_id=media.file_id,
            duration_s=media.duration_s,
            matches=matches,
            families=families,
            postings_scanned=total_scanned,
        )

    @staticmethod
    def _merge_families(matches: list[Match]) -> list[Family]:
        """One family per physical segment; format versions never double count."""
        buckets: dict[str, list[Match]] = defaultdict(list)
        for match in matches:
            buckets[match.physical_key].append(match)
        families: list[Family] = []
        for physical_key, members in buckets.items():
            members.sort(key=lambda m: (-m.confidence, m.file_id))
            best = members[0]
            family_conf = max(m.confidence for m in members)
            families.append(Family(
                physical_key=physical_key,
                group_id=best.group_id,
                confidence=round(family_conf, 6),
                start_s=min(m.start_s for m in members),
                end_s=max(m.end_s for m in members),
                members=members,
            ))
        families.sort(key=lambda f: (-f.confidence, f.physical_key))
        return families
