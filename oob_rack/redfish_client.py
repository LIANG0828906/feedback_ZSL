from __future__ import annotations

import base64
import json
import socket
import ssl
import time
import urllib.error
import urllib.request

from .errors import ProtocolError


class RedfishClient:
    def __init__(
        self,
        host: str,
        port: int,
        username: str,
        password: str,
        ca_file: str,
        *,
        timeout: float = 0.8,
        retries: int = 1,
    ):
        self.host = host
        self.port = port
        self.timeout = timeout
        self.retries = retries
        self.base_url = f"https://{host}:{port}"
        self.context = ssl.create_default_context(cafile=ca_file)
        self.opener = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPSHandler(context=self.context),
        )
        credential = base64.b64encode(f"{username}:{password}".encode("utf-8")).decode("ascii")
        self.authorization = f"Basic {credential}"

    def _get(self, path: str) -> dict:
        last_error: Exception | None = None
        for attempt in range(self.retries + 1):
            request = urllib.request.Request(self.base_url + path, method="GET")
            request.add_header("Authorization", self.authorization)
            request.add_header("Accept", "application/json")
            try:
                with self.opener.open(request, timeout=self.timeout) as response:
                    raw = response.read(2 * 1024 * 1024)
                    document = json.loads(raw.decode("utf-8"))
                    if not isinstance(document, dict):
                        raise ValueError("Redfish response is not an object")
                    return document
            except urllib.error.HTTPError as exc:
                last_error = ProtocolError(
                    "redfish", f"HTTP {exc.code} for {path}", details={"status": exc.code}
                )
                if exc.code in (401, 403, 404):
                    break
            except (urllib.error.URLError, socket.timeout, ssl.SSLError, ValueError, OSError) as exc:
                last_error = ProtocolError("redfish", f"request {path} failed: {exc}")
            if attempt < self.retries:
                time.sleep(0.03)
        if isinstance(last_error, ProtocolError):
            raise last_error
        raise ProtocolError("redfish", f"request {path} failed")

    def collect_payload(self) -> dict:
        paths = {
            "system": "/redfish/v1/Systems/1",
            "chassis": "/redfish/v1/Chassis/1",
            "manager": "/redfish/v1/Managers/BMC",
            "power": "/redfish/v1/Chassis/1/Power",
            "thermal": "/redfish/v1/Chassis/1/Thermal",
        }
        documents = {name: self._get(path) for name, path in paths.items()}
        system = documents["system"]
        manager = documents["manager"]
        thermal = documents["thermal"]
        sensors = []
        for entry in thermal.get("Temperatures", []) or []:
            if not isinstance(entry, dict):
                continue
            sensors.append(
                {
                    "name": entry.get("Name") or entry.get("MemberId") or "temperature",
                    "kind": "temperature",
                    "unit": "C",
                    "reading": entry.get("ReadingCelsius"),
                    "raw_entry": entry,
                }
            )
        for entry in thermal.get("Fans", []) or []:
            if not isinstance(entry, dict):
                continue
            sensors.append(
                {
                    "name": entry.get("Name") or entry.get("MemberId") or "fan",
                    "kind": "fan",
                    "unit": entry.get("ReadingUnits") or "RPM",
                    "reading": entry.get("Reading"),
                    "raw_entry": entry,
                }
            )
        return {
            "asset": {
                "manufacturer": system.get("Manufacturer")
                or documents["chassis"].get("Manufacturer")
                or "",
                "model": system.get("Model") or documents["chassis"].get("Model") or "",
                "serial": system.get("SerialNumber")
                or documents["chassis"].get("SerialNumber")
                or "",
                "firmware_version": manager.get("FirmwareVersion") or "",
            },
            "power_raw": {
                "document": documents["power"],
                "path": "/redfish/v1/Chassis/1/Power",
            },
            "sensors": sensors,
            "raw": {
                "system": system,
                "chassis": documents["chassis"],
                "manager": manager,
                "power": documents["power"],
                "thermal": thermal,
            },
        }
