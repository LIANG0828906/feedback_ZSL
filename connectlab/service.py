from __future__ import annotations

import asyncio
import logging
import signal
import sys
from pathlib import Path
from typing import Any

from .api import ApiServer
from .config import load_config, resolve_path
from .db import Database
from .events import EventStore
from .injection import InjectionController
from .proxy import ProxyServer
from .runner import Runner
from .upstream import UpstreamServer
from .util import atomic_write_json, utc_now
from .workspace import ensure_workspace


LOGGER = logging.getLogger("connectlab.service")


class ConnectLabService:
    def __init__(self, config: dict[str, Any]) -> None:
        self.config = config
        self.events = EventStore()
        self.database = Database(resolve_path(str(config["service"]["database"])))
        self.injections = InjectionController(
            resolve_path(str(config["injection"]["file"])),
            config["injection"]["default_id"],
        )
        self.proxy = ProxyServer(config, self.events)
        self.upstream = UpstreamServer(config, self.events)
        self.runner = Runner(
            config,
            self.database,
            self.events,
            self.injections,
            self.proxy,
        )
        self.api = ApiServer(config, self.database, self.injections, self.runner)
        self._stopping = asyncio.Event()

    async def start(self) -> None:
        ensure_workspace(self.config)
        await self.upstream.start()
        await self.proxy.start()
        await self.api.start()
        LOGGER.info(
            "ConnectLab ready: proxy=%s:%s upstream=%s:%s api=%s:%s",
            self.config["proxy"]["listen_host"],
            self.config["proxy"]["listen_port"],
            self.config["upstream"]["listen_host"],
            self.config["upstream"]["listen_port"],
            self.config["service"]["host"],
            self.config["service"]["port"],
        )

    async def stop(self) -> None:
        self._stopping.set()
        await self.api.stop()
        await self.proxy.stop()
        await self.upstream.stop()

    async def run_forever(self) -> None:
        await self.start()
        loop = asyncio.get_running_loop()
        for signame in ("SIGINT", "SIGTERM"):
            signal_value = getattr(signal, signame, None)
            if signal_value is not None:
                try:
                    loop.add_signal_handler(signal_value, self._stopping.set)
                except (NotImplementedError, RuntimeError):
                    pass
        await self._stopping.wait()
        await self.stop()


def configure_logging(config: dict[str, Any]) -> None:
    level = getattr(logging, str(config["service"].get("log_level", "INFO")).upper(), logging.INFO)
    root = Path(config["_root"])
    (root / "logs").mkdir(parents=True, exist_ok=True)
    logging.basicConfig(
        level=level,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
        handlers=[
            logging.StreamHandler(sys.stdout),
            logging.FileHandler(root / "logs" / "connectlab.log", encoding="utf-8"),
        ],
    )


async def serve(config_path: str | None = None) -> None:
    config, _path = load_config(config_path)
    configure_logging(config)
    ensure_workspace(config)
    runtime_path = Path(config["_root"]) / "var" / "runtime.json"
    atomic_write_json(
        runtime_path,
        {
            "api": f"http://{config['service']['host']}:{config['service']['port']}",
            "proxy": f"{config['proxy']['listen_host']}:{config['proxy']['listen_port']}",
            "upstream": f"{config['upstream']['listen_host']}:{config['upstream']['listen_port']}",
            "started_at": utc_now(),
        },
    )
    service = ConnectLabService(config)
    await service.run_forever()
