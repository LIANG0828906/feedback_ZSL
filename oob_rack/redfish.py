from __future__ import annotations

import base64
import hmac
import json
import socket
import ssl
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

from .emulator_state import BmcRuntime
from .errors import PortInUseError


_POWER_LABELS = {"on": "On", "off": "Off", "unknown": "Unknown"}


class RedfishHandler(BaseHTTPRequestHandler):
    server_version = "OobRedfish/1.0"
    sys_version = ""
    protocol_version = "HTTP/1.1"

    @property
    def runtime(self) -> BmcRuntime:
        return self.server.runtime  # type: ignore[attr-defined]

    @property
    def credentials(self) -> tuple[str, str]:
        return self.server.credentials  # type: ignore[attr-defined]

    def log_message(self, format, *args):
        return

    def _authorized(self) -> bool:
        header = self.headers.get("Authorization", "")
        if not header.startswith("Basic "):
            return False
        try:
            decoded = base64.b64decode(header[6:], validate=True).decode("utf-8")
        except Exception:
            return False
        username, separator, password = decoded.partition(":")
        expected_user, expected_password = self.credentials
        return bool(separator) and hmac.compare_digest(username, expected_user) and hmac.compare_digest(
            password, expected_password
        )

    def _send_json(self, status: int, body: dict) -> None:
        encoded = json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(encoded)

    def _unauthorized(self) -> None:
        body = json.dumps(
            {"error": {"code": "Base.1.0.GeneralError", "message": "authentication required"}},
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
        self.send_response(401)
        self.send_header("WWW-Authenticate", 'Basic realm="Redfish"')
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.runtime.is_offline("redfish"):
            self.close_connection = True
            return
        if not self._authorized():
            self._unauthorized()
            return
        path = urlparse(self.path).path.rstrip("/") or "/redfish/v1"
        asset = self.runtime.asset
        if path in ("/redfish/v1", "/redfish/v1/"):
            self._send_json(
                200,
                {
                    "@odata.id": "/redfish/v1",
                    "@odata.type": "#ServiceRoot.v1_13_0.ServiceRoot",
                    "Id": "RootService",
                    "Name": "OOB Rack Simulation",
                    "RedfishVersion": "1.15.0",
                    "Systems": {"@odata.id": "/redfish/v1/Systems"},
                    "Chassis": {"@odata.id": "/redfish/v1/Chassis"},
                    "Managers": {"@odata.id": "/redfish/v1/Managers"},
                },
            )
            return
        if path == "/redfish/v1/Systems":
            self._send_json(
                200,
                {
                    "@odata.id": "/redfish/v1/Systems",
                    "@odata.type": "#ComputerSystemCollection.ComputerSystemCollection",
                    "Name": "Systems",
                    "Members@odata.count": 1,
                    "Members": [{"@odata.id": "/redfish/v1/Systems/1"}],
                },
            )
            return
        if path == "/redfish/v1/Systems/1":
            power = self.runtime.power("redfish")
            self._send_json(
                200,
                {
                    "@odata.id": "/redfish/v1/Systems/1",
                    "@odata.type": "#ComputerSystem.v1_19_0.ComputerSystem",
                    "Id": "1",
                    "Name": asset["model"],
                    "Manufacturer": asset["manufacturer"],
                    "Model": asset["model"],
                    "SerialNumber": asset["serial"],
                    "PartNumber": asset["model"],
                    "PowerState": "On" if power == "on" else "Off" if power == "off" else "Unknown",
                    "Status": {"State": "Enabled", "Health": "OK"},
                    "Chassis": [{"@odata.id": "/redfish/v1/Chassis/1"}],
                },
            )
            return
        if path == "/redfish/v1/Chassis":
            self._send_json(
                200,
                {
                    "@odata.id": "/redfish/v1/Chassis",
                    "@odata.type": "#ChassisCollection.ChassisCollection",
                    "Name": "Chassis",
                    "Members@odata.count": 1,
                    "Members": [{"@odata.id": "/redfish/v1/Chassis/1"}],
                },
            )
            return
        if path == "/redfish/v1/Chassis/1":
            self._send_json(
                200,
                {
                    "@odata.id": "/redfish/v1/Chassis/1",
                    "@odata.type": "#Chassis.v1_25_0.Chassis",
                    "Id": "1",
                    "Name": f"Chassis {asset['slot']}",
                    "Manufacturer": asset["manufacturer"],
                    "Model": asset["model"],
                    "SerialNumber": asset["serial"],
                    "Power": {"@odata.id": "/redfish/v1/Chassis/1/Power"},
                    "Thermal": {"@odata.id": "/redfish/v1/Chassis/1/Thermal"},
                    "Status": {"State": "Enabled", "Health": "OK"},
                },
            )
            return
        if path == "/redfish/v1/Chassis/1/Power":
            self._send_json(200, self._power_document())
            return
        if path == "/redfish/v1/Chassis/1/Thermal":
            self._send_json(200, self._thermal_document())
            return
        if path == "/redfish/v1/Managers":
            self._send_json(
                200,
                {
                    "@odata.id": "/redfish/v1/Managers",
                    "@odata.type": "#ManagerCollection.ManagerCollection",
                    "Name": "Managers",
                    "Members@odata.count": 1,
                    "Members": [{"@odata.id": "/redfish/v1/Managers/BMC"}],
                },
            )
            return
        if path == "/redfish/v1/Managers/BMC":
            self._send_json(
                200,
                {
                    "@odata.id": "/redfish/v1/Managers/BMC",
                    "@odata.type": "#Manager.v1_16_0.Manager",
                    "Id": "BMC",
                    "Name": "BMC",
                    "Manufacturer": asset["manufacturer"],
                    "Model": asset["model"],
                    "SerialNumber": asset["serial"],
                    "FirmwareVersion": asset["firmware_version"],
                    "Status": {"State": "Enabled", "Health": "OK"},
                },
            )
            return
        if path == "/redfish/v1/Chassis/1/ResetActionInfo":
            self._send_json(
                200,
                {
                    "@odata.id": path,
                    "@odata.type": "#ActionInfo.v1_2_0.ActionInfo",
                    "Id": "ResetActionInfo",
                    "Name": "Chassis Reset Action Info",
                    "Parameters": [
                        {
                            "Name": "ResetType",
                            "Required": True,
                            "DataType": "String",
                            "AllowableValues": ["On", "ForceOff", "GracefulRestart"],
                        }
                    ],
                },
            )
            return
        self._send_json(
            404,
            {
                "error": {
                    "code": "Base.1.0.ResourceMissingAtURI",
                    "message": "resource not found",
                }
            },
        )

    def do_POST(self):
        if self.runtime.is_offline("redfish"):
            self.close_connection = True
            return
        if not self._authorized():
            self._unauthorized()
            return
        path = urlparse(self.path).path.rstrip("/")
        if path == "/redfish/v1/Chassis/1/Actions/Chassis.Reset":
            self.send_response(204)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self._send_json(
            404,
            {
                "error": {
                    "code": "Base.1.0.ResourceMissingAtURI",
                    "message": "resource not found",
                }
            },
        )

    def _power_document(self) -> dict:
        power = self.runtime.power("redfish")
        label = _POWER_LABELS.get(power, "Unknown")
        psus = []
        for psu in self.runtime.power_supplies():
            psus.append(
                {
                    "MemberId": psu["member_id"],
                    "Name": psu["name"],
                    "PowerCapacityWatts": 800,
                    "LastPowerOutputWatts": psu["watts"],
                    "Status": {"State": psu["state"], "Health": psu["health"]},
                }
            )
        shape = self.runtime.profile["power_state_shape"]
        if self.runtime.redfish_power_is_unparsable():
            power_state = {"Unexpected": {"Descriptor": "vendor-extension"}}
        elif shape == "object":
            power_state = {
                "State": label,
                "Health": "OK",
                "Source": "PowerControl",
            }
        else:
            power_state = label
        document = {
            "@odata.id": "/redfish/v1/Chassis/1/Power",
            "@odata.type": "#Power.v1_8_0.Power",
            "Id": "Power",
            "Name": "Power",
            "PowerControl": [
                {
                    "MemberId": "0",
                    "Name": "System Power Control",
                    "PowerConsumedWatts": sum(
                        item["LastPowerOutputWatts"] for item in psus
                    ),
                    "PowerRequestedWatts": 650,
                    "PowerState": power_state,
                }
            ],
            "PowerSupplies": psus,
        }
        if shape == "object":
            document["Actions"] = {
                "#Chassis.Reset": {
                    "target": "/redfish/v1/Chassis/1/Actions/Chassis.Reset",
                    "@Redfish.ActionInfo": "/redfish/v1/Chassis/1/ResetActionInfo",
                }
            }
        return document

    def _thermal_document(self) -> dict:
        values = self.runtime.sensors("redfish")
        temperatures = []
        fans = []
        for index, name in enumerate(("Inlet Temp", "CPU Temp"), start=1):
            if self.runtime.redfish_sensor_is_unparsable(name):
                reading = {"Unexpected": {"Reading": values[name]}}
            else:
                reading = values[name]
            temperatures.append(
                {
                    "MemberId": f"Temp{index}",
                    "Name": name,
                    "ReadingCelsius": reading,
                    "UpperThresholdCritical": 70,
                    "LowerThresholdCritical": 5,
                    "Status": {"State": "Enabled", "Health": "OK"},
                }
            )
        for index, name in enumerate(("Fan 1", "Fan 2", "Fan 3", "Fan 4"), start=1):
            if self.runtime.redfish_sensor_is_unparsable(name):
                reading = {"Unexpected": {"Reading": values[name]}}
            else:
                reading = values[name]
            fans.append(
                {
                    "MemberId": f"Fan{index}",
                    "Name": name,
                    "Reading": reading,
                    "ReadingUnits": "RPM",
                    "Status": {"State": "Enabled", "Health": "OK"},
                }
            )
        return {
            "@odata.id": "/redfish/v1/Chassis/1/Thermal",
            "@odata.type": "#Thermal.v1_7_0.Thermal",
            "Id": "Thermal",
            "Name": "Thermal",
            "Temperatures": temperatures,
            "Fans": fans,
        }


class RedfishListener:
    def __init__(self, runtime: BmcRuntime, host: str, port: int, credentials: tuple[str, str], ssl_context: ssl.SSLContext):
        self.runtime = runtime
        try:
            self.server = ThreadingHTTPServer((host, port), RedfishHandler)
        except OSError as exc:
            raise PortInUseError(
                f"cannot bind Redfish HTTPS {host}:{port}",
                details={"transport": "tcp", "host": host, "port": port},
            ) from exc
        self.server.daemon_threads = True
        self.server.runtime = runtime  # type: ignore[attr-defined]
        self.server.credentials = credentials  # type: ignore[attr-defined]
        self.server.socket = ssl_context.wrap_socket(self.server.socket, server_side=True)
        self.started = False

    def start(self) -> None:
        import threading

        self.thread = threading.Thread(
            target=self.server.serve_forever,
            name=f"redfish-{self.runtime.device_id}",
            daemon=True,
        )
        self.thread.start()
        self.started = True

    def stop(self) -> None:
        if self.started:
            self.server.shutdown()
        self.started = False
        self.server.server_close()
