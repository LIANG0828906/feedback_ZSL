"""Varnish 7.4 lifecycle: locate varnishd (repo-local, env, PATH, or docker),
start it with the generated VCL, and drive the admin CLI."""
from __future__ import annotations

import asyncio
import os
import re
import shutil
import subprocess
from pathlib import Path

from .config import Config


class TopologyError(Exception):
    def __init__(self, detail: str):
        super().__init__(detail)
        self.detail = detail
        self.error_code = "E_TOPOLOGY"


REPO_VARNISH = Path(__file__).resolve().parent.parent / ".runtime" / "varnish"


def _find_local_varnishd(config: Config) -> tuple[str, Path] | None:
    candidates = []
    if config.varnishd_path:
        candidates.append(Path(config.varnishd_path))
    env = os.environ.get("EDGECHECK_VARNISHD")
    if env:
        candidates.append(Path(env))
    candidates.append(REPO_VARNISH / "usr" / "sbin" / "varnishd")
    which = shutil.which("varnishd")
    if which:
        candidates.append(Path(which))
    for cand in candidates:
        if cand.is_file() and os.access(cand, os.X_OK):
            root = cand.parent.parent.parent  # <root>/usr/sbin/varnishd
            return str(cand), root
    return None


def varnish_version(varnishd: str, env: dict) -> str:
    out = subprocess.run([varnishd, "-V"], capture_output=True, text=True,
                         env=env)
    text = (out.stdout or "") + (out.stderr or "")
    match = re.search(r"varnish-(\d+\.\d+\.\d+)", text)
    return match.group(1) if match else "unknown"


class VarnishInstance:
    def __init__(self, config: Config):
        self.config = config
        self.mode = "local"
        self.varnishd: str | None = None
        self.root: Path | None = None
        self.version = "unknown"
        self.proc: subprocess.Popen | None = None
        self.work_dir = Path(config.work_dir).resolve()
        self.env = dict(os.environ)

    def resolve(self) -> None:
        found = _find_local_varnishd(self.config)
        if found is None:
            raise TopologyError(
                "varnishd not found (set EDGECHECK_VARNISHD or provide "
                ".runtime/varnish)")
        self.varnishd, self.root = found
        lib = self.root / "usr" / "lib"
        if lib.is_dir():
            self.env["LD_LIBRARY_PATH"] = (
                f"{lib}:{self.env.get('LD_LIBRARY_PATH', '')}")
        self.version = varnish_version(self.varnishd, self.env)
        if not self.version.startswith("7.4"):
            raise TopologyError(
                f"varnishd version {self.version} is not 7.4.x")

    def start(self, vcl_file: str) -> None:
        assert self.varnishd is not None and self.root is not None
        if self.work_dir.exists():
            shutil.rmtree(self.work_dir, ignore_errors=True)
        self.work_dir.mkdir(parents=True, exist_ok=True)
        vmod_path = self.root / "usr" / "lib" / "varnish" / "vmods"
        vcl_path = ":".join([
            str(self.root / "usr" / "share" / "varnish" / "vcc"),
            str(self.root / "usr" / "include" / "varnish"),
        ])
        cmd = [
            self.varnishd, "-F",
            "-n", str(self.work_dir),
            "-a", f"{self.config.bind}:{self.config.front_port}",
            "-T", f"{self.config.bind}:{self.config.admin_port}",
            "-f", vcl_file,
            "-s", "malloc,64m",
            "-p", "feature=+http2",
            "-p", "http_gzip_support=off",
            "-p", "thread_pools=1",
            "-p", "thread_pool_min=5",
            "-p", "thread_pool_max=100",
            "-p", f"vmod_path={vmod_path}",
            "-p", f"vcl_path={vcl_path}",
        ]
        self._log = open(self.work_dir.parent / "varnish-stderr.log", "wb")
        self.proc = subprocess.Popen(
            cmd, env=self.env, stdout=self._log, stderr=self._log)

    async def wait_ready(self, timeout: float = 20.0) -> None:
        loop = asyncio.get_event_loop()
        deadline = loop.time() + timeout
        while loop.time() < deadline:
            if self.proc is not None and self.proc.poll() is not None:
                raise TopologyError(
                    f"varnishd exited early (rc={self.proc.returncode})")
            try:
                _, writer = await asyncio.wait_for(
                    asyncio.open_connection(
                        self.config.bind, self.config.front_port), 1.0)
                writer.close()
                front_ok = True
            except (OSError, asyncio.TimeoutError):
                front_ok = False
            if front_ok and self.admin_ok("ping"):
                return
            await asyncio.sleep(0.2)
        raise TopologyError("varnishd did not become ready")

    def admin(self, *args: str) -> tuple[bool, str]:
        varnishadm = self.root / "usr" / "bin" / "varnishadm"
        if not varnishadm.is_file():
            varnishadm = shutil.which("varnishadm") or "varnishadm"
        cmd = [str(varnishadm), "-n", str(self.work_dir), *args]
        out = subprocess.run(cmd, capture_output=True, text=True,
                             env=self.env, timeout=15)
        return out.returncode == 0, (out.stdout or "") + (out.stderr or "")

    def admin_ok(self, *args: str) -> bool:
        try:
            ok, _ = self.admin(*args)
            return ok
        except Exception:
            return False

    def load_vcl(self, name: str, vcl_file: str) -> None:
        ok, out = self.admin("vcl.load", name, vcl_file)
        if not ok:
            raise TopologyError(f"vcl.load failed: {out.strip()}")
        ok, out = self.admin("vcl.use", name)
        if not ok:
            raise TopologyError(f"vcl.use failed: {out.strip()}")

    def active_vcl(self) -> str:
        ok, out = self.admin("vcl.list")
        if ok:
            for line in out.splitlines():
                if line.startswith("active"):
                    return line.split()[-1]
        return "unknown"

    def stop(self) -> None:
        if self.proc is None:
            return
        try:
            self.admin("stop")
        except Exception:
            pass
        try:
            self.proc.terminate()
            self.proc.wait(timeout=10)
        except Exception:
            try:
                self.proc.kill()
            except Exception:
                pass
        self.proc = None
        log = getattr(self, "_log", None)
        if log is not None:
            try:
                log.close()
            except Exception:
                pass
            self._log = None
