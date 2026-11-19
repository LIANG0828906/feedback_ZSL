from __future__ import annotations

import threading
import time

from .collector import RackCollector
from .config import ConfigManager
from .storage import RackStorage, utc_now


class InspectorService:
    def __init__(self, config_manager: ConfigManager, storage: RackStorage):
        self.config_manager = config_manager
        self.storage = storage
        self._stop = threading.Event()
        self._cycle_lock = threading.Lock()
        self._thread: threading.Thread | None = None
        self.last_cycle: dict | None = None
        self.last_error: str | None = None

    def _run_one_cycle(self) -> dict:
        with self._cycle_lock:
            config = self.config_manager.config
            collector = RackCollector(config)
            device_ids = [device["device_id"] for device in config["devices"]]
            state = self.storage.start_or_resume_inspection(
                rack_id=config["rack_id"],
                fingerprint=self.config_manager.fingerprint,
                config_version=self.config_manager.generation,
                expected_devices=len(device_ids),
            )
            inspection_id = state["inspection_id"]
            committed = self.storage.committed_device_ids(inspection_id)
            missing = set(device_ids) - committed
            if missing:
                collected = collector.collect_rack(missing)
                for aggregate in collected["devices"]:
                    self.storage.record_device_result(
                        inspection_id,
                        aggregate,
                        observed_at=utc_now(),
                    )
            committed = self.storage.committed_device_ids(inspection_id)
            if committed != set(device_ids):
                missing_after = sorted(set(device_ids) - committed)
                raise RuntimeError(f"inspection incomplete; missing devices: {missing_after}")
            completion = self.storage.complete_inspection(inspection_id)
            status = self.storage.latest_status(config["rack_id"])
            result = {
                "inspection_id": inspection_id,
                "resumed": state["resumed"],
                "conclusion": completion["conclusion"],
                "completed_at": completion["completed_at"],
                "device_count": len(device_ids),
            }
            self.last_cycle = result
            self.last_error = None
            _ = status
            return result

    def run_once(self) -> dict:
        return self._run_one_cycle()

    def start(self) -> None:
        if self._thread is not None and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(
            target=self._loop, name="oob-inspector", daemon=True
        )
        self._thread.start()

    def _loop(self) -> None:
        config = self.config_manager.config
        if not config["inspection"].get("start_immediately", True):
            self._stop.wait(float(config["inspection"]["interval_seconds"]))
        while not self._stop.is_set():
            try:
                self._run_one_cycle()
            except Exception as exc:
                self.last_error = f"{type(exc).__name__}: {exc}"
            interval = float(self.config_manager.config["inspection"]["interval_seconds"])
            self._stop.wait(interval)

    def stop(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=15)
            self._thread = None

    def reload_config(self) -> dict:
        config = self.config_manager.reload()
        return {
            "reloaded": True,
            "generation": self.config_manager.generation,
            "fingerprint": self.config_manager.fingerprint,
            "interval_seconds": config["inspection"]["interval_seconds"],
        }
