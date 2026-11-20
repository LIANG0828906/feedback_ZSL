"""Cross-version fingerprint migration (v1/v2 -> v3).

Migration preserves original peak pairs and the original time quantization
baseline (in the v3 `legacy` block). Every field mapping is recorded, and
any field that cannot be expressed losslessly in the target version is
flagged with E_MIGRATE_LOSS in the report -- never silently dropped.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from .errors import E_FMT_INVALID, E_MIGRATE_LOSS, FpError
from .model import BAND_EDGES_HZ, TIME_QUANT_MS, Fingerprint


@dataclass
class MigrationReport:
    sample_id: str
    source_version: int
    target_version: int
    field_mappings: list = field(default_factory=list)
    lost_fields: list = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.lost_fields

    def map(self, source, target, action):
        self.field_mappings.append(
            {"source": source, "target": target, "action": action})

    def loss(self, field_name, reason, count=1):
        self.lost_fields.append({
            "code": E_MIGRATE_LOSS,
            "field": field_name,
            "reason": reason,
            "count": count,
        })

    def to_dict(self):
        return {
            "sample_id": self.sample_id,
            "source_version": self.source_version,
            "target_version": self.target_version,
            "ok": self.ok,
            "field_mappings": self.field_mappings,
            "lost_fields": self.lost_fields,
        }


def _requantize_hashes(fp, target_quant, report):
    """Re-express hash times on the unified quant grid; flag off-grid loss."""
    new_hashes = []
    off_grid = 0
    max_residual = 0.0
    for h, t in fp.hashes:
        tq = round(round(t / target_quant) * target_quant, 6)
        residual = abs(tq - t)
        if residual > 1e-6:
            off_grid += 1
            max_residual = max(max_residual, residual)
        new_hashes.append((h, tq))
    if off_grid:
        report.loss(
            "hashes[].t_ms",
            f"{off_grid} hash times off the {target_quant}ms grid "
            f"(max residual {max_residual:.3f}ms); originals kept in legacy",
            count=off_grid,
        )
    return new_hashes


def migrate_fingerprint(fp, target_version: int = 3):
    """Migrate a parsed fingerprint to v3. Returns (new_fp, MigrationReport)."""
    if target_version != 3:
        raise FpError(E_FMT_INVALID,
                      f"unsupported target version {target_version}",
                      field="target_version")
    if fp.source_version == 3:
        raise FpError(E_FMT_INVALID, "fingerprint is already v3",
                      field="format_version")

    report = MigrationReport(fp.sample_id, fp.source_version, target_version)
    new = Fingerprint(
        sample_id=fp.sample_id,
        sample_rate=fp.sample_rate,
        channels=fp.channels,
        duration_ms=fp.duration_ms,
        frame_length=fp.frame_length,
        hop_length=fp.hop_length,
        time_quant_ms=TIME_QUANT_MS,
        peak_pairs=list(fp.peak_pairs),
        bands_hz=tuple(fp.bands_hz) or BAND_EDGES_HZ,
        channel_merge=(fp.channels == 2),
        compression=None,
        source_version=3,
    )

    report.map("payload.sample_id", "payload.sample_id", "copied")
    report.map("payload.sample_rate", "payload.sample_rate", "copied")
    report.map("payload.channels", "payload.channels", "copied")
    report.map("payload.duration_ms", "payload.duration_ms", "copied")
    report.map("payload.frame_length", "payload.frame_length", "copied")
    report.map("payload.hop_length", "payload.hop_length", "copied")
    report.map("payload.peak_pairs", "payload.peak_pairs", "preserved-raw")

    if fp.source_version == 1:
        report.map("(none)", "payload.time_quant_ms",
                   f"defaulted to unified baseline {TIME_QUANT_MS}ms")
        report.map("(none)", "payload.bands_hz", "defaulted to canonical bands")
        report.map("(none)", "payload.channel_merge",
                   "derived from channels==2")
        report.map("(none)", "payload.compression", "defaulted to null")
        report.loss("payload.channel_merge",
                    "v1 has no channel-merge flag; derived from channel "
                    "count, original merge state unrecoverable")
        new.hashes = _requantize_hashes(fp, TIME_QUANT_MS, report)
        new.legacy = {
            "source_version": 1,
            "time_quant_ms": None,
            "quant_bits": None,
            "extras": {"original_hash_times_ms": [t for _, t in fp.hashes]},
        }
    elif fp.source_version == 2:
        report.map("payload.time_quant_ms", "payload.time_quant_ms",
                   f"rebased to unified {TIME_QUANT_MS}ms; original kept in "
                   "legacy.time_quant_ms")
        report.map("payload.bands_hz", "payload.bands_hz", "copied")
        report.map("payload.quant_bits", "payload.legacy.quant_bits",
                   "no v3 field; preserved in legacy block")
        report.map("payload.segment_hashes", "payload.hashes",
                   "re-expressed as (hash, t_ms)")
        report.map("(none)", "payload.channel_merge",
                   "derived from channels==2")
        report.map("(none)", "payload.compression", "defaulted to null")
        report.loss("payload.quant_bits",
                    "v2 quant_bits has no v3 counterpart; value preserved "
                    "in legacy.quant_bits but no longer active")
        report.loss("payload.segment_hashes[].segment",
                    "v2 segment index is derivable from t_ms and is not "
                    "stored in v3; originals kept in legacy.extras")
        new.hashes = _requantize_hashes(fp, TIME_QUANT_MS, report)
        new.legacy = {
            "source_version": 2,
            "time_quant_ms": fp.time_quant_ms,
            "quant_bits": fp.quant_bits,
            "extras": {
                "original_hash_times_ms": [t for _, t in fp.hashes],
                "segments": [int(t // 1000.0) for _, t in fp.hashes],
            },
        }
    else:
        raise FpError(E_FMT_INVALID,
                      f"cannot migrate from version {fp.source_version}",
                      field="format_version")

    new.provenance = dict(fp.provenance)
    new.provenance["migrated_from"] = fp.source_version
    return new, report
