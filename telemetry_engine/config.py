"""Configuration loading and validation."""

import json
import os


class ConfigError(Exception):
    pass


def load_config(path):
    with open(path, "r", encoding="utf-8") as fh:
        cfg = json.load(fh)
    for key in ("data_dir", "output_dir", "state_dir", "seed",
                "generator", "engine", "tenants"):
        if key not in cfg:
            raise ConfigError(f"missing config key: {key}")
    eng = cfg["engine"]
    for key in ("worker_processes", "units_per_tenant", "window_seconds",
                "allowed_lateness_seconds", "skew_threshold_rows", "max_retries"):
        if key not in eng:
            raise ConfigError(f"missing engine config key: {key}")
    if not cfg["tenants"]:
        raise ConfigError("at least one tenant must be declared")
    for tenant in cfg["tenants"]:
        for key in ("tenant_id", "fleet", "memory_quota_bytes", "max_units"):
            if key not in tenant:
                raise ConfigError(f"tenant missing key: {key}")
    cfg.setdefault("fault_injection", {"crash": [], "always_fail": []})
    cfg["_base_dir"] = os.path.dirname(os.path.abspath(path))
    return cfg


def resolve(cfg, *parts):
    return os.path.join(cfg["_base_dir"], *parts)
