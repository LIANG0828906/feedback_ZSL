from __future__ import annotations

import os
from dataclasses import dataclass


@dataclass(frozen=True)
class Config:
    bind: str = "127.0.0.1"
    seed: int = 273
    url_count: int = 60
    front_port: int = 27380
    backend_a_port: int = 27381
    backend_b_port: int = 27382
    backend_c_port: int = 27383
    bridge_port: int = 27384
    admin_port: int = 27385
    varnish_mode: str = "auto"  # auto | local | docker
    varnishd_path: str | None = None
    work_dir: str = ".runtime/work"
    vcl_out: str = ".runtime/edgecheck.vcl"
    report_path: str = "artifacts/compatibility-report.json"
    cache_ttl: int = 300
    request_timeout: float = 15.0

    @property
    def front_authority(self) -> str:
        return f"{self.bind}:{self.front_port}"

    def all_ports(self) -> dict[str, int]:
        return {
            "varnish_front": self.front_port,
            "backend_a": self.backend_a_port,
            "backend_b": self.backend_b_port,
            "backend_c": self.backend_c_port,
            "backend_c_bridge": self.bridge_port,
            "varnish_admin": self.admin_port,
        }


def load_config(args=None) -> Config:
    """Build Config from defaults + EDGECHECK_* env + optional argparse args."""
    cfg = Config()
    overrides = {}
    env_map = {
        "EDGECHECK_SEED": ("seed", int),
        "EDGECHECK_FRONT_PORT": ("front_port", int),
        "EDGECHECK_BACKEND_A_PORT": ("backend_a_port", int),
        "EDGECHECK_BACKEND_B_PORT": ("backend_b_port", int),
        "EDGECHECK_BACKEND_C_PORT": ("backend_c_port", int),
        "EDGECHECK_BRIDGE_PORT": ("bridge_port", int),
        "EDGECHECK_ADMIN_PORT": ("admin_port", int),
        "EDGECHECK_VARNISH_MODE": ("varnish_mode", str),
        "EDGECHECK_VARNISHD": ("varnishd_path", str),
        "EDGECHECK_REPORT": ("report_path", str),
    }
    for env, (attr, conv) in env_map.items():
        val = os.environ.get(env)
        if val:
            overrides[attr] = conv(val)
    if args is not None:
        for attr in (
            "seed", "front_port", "backend_a_port", "backend_b_port",
            "backend_c_port", "bridge_port", "admin_port", "varnish_mode",
            "varnishd_path", "report_path",
        ):
            val = getattr(args, attr, None)
            if val is not None:
                overrides[attr] = val
    if overrides:
        cfg = Config(**{**cfg.__dict__, **overrides})
    return cfg
