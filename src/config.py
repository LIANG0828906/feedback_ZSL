"""Configuration loading for the replication-slot guard service."""
from __future__ import annotations

import json
import os
from dataclasses import dataclass, fields
from pathlib import Path
from typing import Any

CODE_ROOT = Path(__file__).resolve().parents[1]
if os.environ.get("SLOTGUARD_ROOT"):
    ROOT = Path(os.environ["SLOTGUARD_ROOT"]).resolve()
elif (CODE_ROOT / "config.json").exists():
    ROOT = CODE_ROOT
else:
    ROOT = Path.cwd()


@dataclass
class Config:
    primary_dsn: str = "host=127.0.0.1 port=55432 user=postgres dbname=postgres"
    replica_dsn: str = "host=127.0.0.1 port=55433 user=postgres dbname=postgres"
    redis_url: str = "redis://127.0.0.1:6379/0"
    redis_key_prefix: str = "uid319:slotguard:"
    publication_name: str = "wms_pub"
    subscription_name: str = "wms_sub"
    slot_name: str = "wms_sub_slot"
    tables: list[str] | None = None
    parent_table: str = "wms.outbound_order"
    child_table: str = "wms.settlement_detail"
    parent_key: str = "order_id"
    child_key: str = "detail_id"
    child_fk: str = "order_id"
    parent_version_column: str = "updated_at"
    child_version_column: str = "updated_at"
    child_constraint_name: str = "settlement_detail_order_id_fkey"
    sqlite_path: str = "var/slotguard.db"
    target_meta_schema: str = "slotguard"
    metrics_host: str = "127.0.0.1"
    metrics_port: int = 9721
    monitor_interval_s: int = 10
    sampling_interval_s: int = 30
    reconcile_interval_s: int = 30
    sample_order_count: int = 20
    sample_seed: int = 319
    resync_batch_size: int = 2000
    resync_wait_timeout_s: int = 300
    resync_truncate_on_new: bool = True
    auto_resume_resync: bool = True
    auto_rebootstrap: bool = True
    resync_retry_interval_s: int = 60
    wal_retention_threshold_bytes: int = 268435456
    relief_recheck_rounds: int = 2
    relief_min_inactive_s: int = 0
    connect_timeout_s: int = 5

    def __post_init__(self) -> None:
        if not self.tables:
            self.tables = [self.parent_table, self.child_table]
        self.tables = list(dict.fromkeys(self.tables))

    @property
    def instances(self) -> dict[str, str]:
        return {"primary": self.primary_dsn, "replica": self.replica_dsn}


_BOOL_FIELDS = {
    "resync_truncate_on_new",
    "auto_resume_resync",
    "auto_rebootstrap",
}
_INT_FIELDS = {
    "metrics_port",
    "monitor_interval_s",
    "sampling_interval_s",
    "reconcile_interval_s",
    "sample_order_count",
    "sample_seed",
    "resync_batch_size",
    "resync_wait_timeout_s",
    "resync_retry_interval_s",
    "wal_retention_threshold_bytes",
    "relief_recheck_rounds",
    "relief_min_inactive_s",
    "connect_timeout_s",
}


def _coerce(key: str, value: Any) -> Any:
    if key in _INT_FIELDS:
        return int(value)
    if key in _BOOL_FIELDS:
        if isinstance(value, bool):
            return value
        return str(value).strip().lower() in {"1", "true", "yes", "on"}
    if key == "tables" and isinstance(value, str):
        parsed = json.loads(value)
        if not isinstance(parsed, list):
            raise ValueError("tables must be a JSON array")
        return parsed
    return value


def load_config(path: str | os.PathLike[str] | None = None) -> Config:
    """Load JSON configuration, then apply SLOTGUARD_<FIELD> overrides."""
    data: dict[str, Any] = {}
    cfg_path = Path(path) if path else Path(
        os.environ.get("SLOTGUARD_CONFIG", str(ROOT / "config.json"))
    )
    if cfg_path.exists():
        data.update(json.loads(cfg_path.read_text(encoding="utf-8")))

    allowed = {f.name for f in fields(Config)}
    for field in allowed:
        env_name = f"SLOTGUARD_{field.upper()}"
        if env_name in os.environ:
            data[field] = os.environ[env_name]
    aliases = {
        "SLOTGUARD_WAL_THRESHOLD": "wal_retention_threshold_bytes",
        "SLOTGUARD_SAMPLE_COUNT": "sample_order_count",
    }
    for env_name, key in aliases.items():
        if env_name in os.environ:
            data[key] = os.environ[env_name]

    data = {k: _coerce(k, v) for k, v in data.items() if k in allowed}
    cfg = Config(**data)
    if not os.path.isabs(cfg.sqlite_path):
        cfg.sqlite_path = str(ROOT / cfg.sqlite_path)
    return cfg
