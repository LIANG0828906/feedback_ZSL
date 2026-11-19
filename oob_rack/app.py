from __future__ import annotations

import json
import signal
import time
from pathlib import Path

from .config import ConfigManager, resolve_path
from .control import ControlServer
from .emulator import BmcEmulator
from .emulator_state import FaultController
from .monitor import MonitorServer
from .service import InspectorService
from .storage import RackStorage


def _wait_forever() -> None:
    stop = False

    def _stop(signum, frame):
        nonlocal stop
        stop = True

    previous = {}
    for name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, name):
            previous[name] = signal.signal(getattr(signal, name), _stop)
    try:
        while not stop:
            time.sleep(0.25)
    finally:
        for name, handler in previous.items():
            signal.signal(getattr(signal, name), handler)


def run_service(config_path: str, *, once: bool = False, with_monitor: bool = True) -> dict:
    manager = ConfigManager(config_path)
    config = manager.config
    storage = RackStorage(resolve_path(config, config["inspection"]["database"]))
    faults = FaultController()
    emulator = BmcEmulator(config, faults)
    control = ControlServer(
        config["control"]["host"], int(config["control"]["port"]), faults
    )
    service = InspectorService(manager, storage)
    monitor = None
    try:
        emulator.start()
        control.start()
        if with_monitor:
            monitor = MonitorServer(
                config["monitor"]["host"], int(config["monitor"]["port"]), service
            )
            monitor.start()
        if once:
            service.run_once()
            return storage.latest_status(config["rack_id"])
        service.start()
        _wait_forever()
        return storage.latest_status(config["rack_id"])
    finally:
        service.stop()
        if monitor is not None:
            monitor.stop()
        control.stop()
        emulator.stop()
        storage.close()


def run_emulator(config_path: str) -> None:
    manager = ConfigManager(config_path)
    config = manager.config
    faults = FaultController()
    emulator = BmcEmulator(config, faults)
    control = ControlServer(
        config["control"]["host"], int(config["control"]["port"]), faults
    )
    try:
        emulator.start()
        control.start()
        _wait_forever()
    finally:
        control.stop()
        emulator.stop()


def run_inspection(config_path: str) -> dict:
    manager = ConfigManager(config_path)
    config = manager.config
    storage = RackStorage(resolve_path(config, config["inspection"]["database"]))
    service = InspectorService(manager, storage)
    try:
        result = service.run_once()
        return {
            "result": result,
            "status": storage.latest_status(config["rack_id"]),
        }
    finally:
        storage.close()
