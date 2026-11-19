from __future__ import annotations

import socket
import ssl
import threading

from .config import resolve_path
from .deterministic import deterministic_guid
from .emulator_state import BmcRuntime, FaultController
from .errors import OobError, PortInUseError
from .ipmi import IpmiServerEndpoint
from .redfish import RedfishListener
from .snmp import SnmpEndpoint, SnmpListener


class UdpDatagramListener:
    def __init__(self, name: str, host: str, port: int, handler):
        self.name = name
        self.handler = handler
        self.socket = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            self.socket.bind((host, port))
        except OSError as exc:
            self.socket.close()
            raise PortInUseError(
                f"cannot bind UDP {host}:{port} ({name})",
                details={"transport": "udp", "host": host, "port": port},
            ) from exc
        self.socket.settimeout(0.25)
        self._stop = threading.Event()
        self.thread = threading.Thread(target=self._run, name=name, daemon=True)
        self.started = False

    def start(self) -> None:
        self.thread.start()
        self.started = True

    def _run(self) -> None:
        while not self._stop.is_set():
            try:
                data, address = self.socket.recvfrom(65535)
            except socket.timeout:
                continue
            except OSError:
                break
            try:
                response = self.handler(data)
            except Exception:
                response = None
            if response:
                try:
                    self.socket.sendto(response, address)
                except OSError:
                    pass

    def stop(self) -> None:
        self._stop.set()
        self.socket.close()
        if self.started:
            self.thread.join(timeout=2)
        self.started = False


class BmcEmulator:
    def __init__(self, config: dict, faults: FaultController | None = None):
        self.config = config
        self.faults = faults or FaultController()
        self.runtimes: dict[str, BmcRuntime] = {}
        self.ipmi_listeners: list[UdpDatagramListener] = []
        self.snmp_listeners: list[SnmpListener] = []
        self.redfish_listeners: list[RedfishListener] = []
        self._started = False
        self._load_runtimes()

    def _load_runtimes(self) -> None:
        for device in self.config["devices"]:
            profile = self.config["firmware_profiles"][device["firmware_profile"]]
            self.runtimes[device["device_id"]] = BmcRuntime(
                device, profile, self.config["seed"], self.faults
            )

    def _ssl_context(self) -> ssl.SSLContext:
        cert_path = resolve_path(self.config, self.config["redfish"]["server_cert"])
        key_path = resolve_path(self.config, self.config["redfish"]["server_key"])
        try:
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.minimum_version = ssl.TLSVersion.TLSv1_2
            context.load_cert_chain(str(cert_path), str(key_path))
            return context
        except (OSError, ssl.SSLError) as exc:
            raise OobError(
                "Redfish TLS certificate or key could not be loaded",
                details={"reason": str(exc)},
            ) from exc

    def start(self) -> None:
        if self._started:
            return
        host = self.config["listen_host"]
        username = self.config["ipmi"]["username"]
        password = self.config["ipmi"]["password"]
        for device in self.config["devices"]:
            protocols = self.config["firmware_profiles"][device["firmware_profile"]]["protocols"]
            if "redfish" not in protocols:
                probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                try:
                    probe.bind((host, device["redfish_port"]))
                except OSError as exc:
                    raise PortInUseError(
                        f"declared legacy Redfish port is occupied: {device['redfish_port']}",
                        details={
                            "transport": "tcp",
                            "host": host,
                            "port": device["redfish_port"],
                        },
                    ) from exc
                finally:
                    probe.close()
        try:
            for device in self.config["devices"]:
                runtime = self.runtimes[device["device_id"]]
                endpoint = IpmiServerEndpoint(
                    runtime,
                    username,
                    password,
                    deterministic_guid(device["device_id"]),
                )
                listener = UdpDatagramListener(
                    f"ipmi-{device['device_id']}",
                    host,
                    device["ipmi_port"],
                    endpoint.handle,
                )
                self.ipmi_listeners.append(listener)

                snmp_endpoint = SnmpEndpoint(
                    runtime,
                    self.config["snmp"]["community"],
                    self.config["snmp"]["enterprise_base"],
                )
                self.snmp_listeners.append(
                    SnmpListener(snmp_endpoint, host, device["snmp_port"])
                )

                protocols = self.config["firmware_profiles"][device["firmware_profile"]]["protocols"]
                if "redfish" in protocols:
                    self.redfish_listeners.append(
                        RedfishListener(
                            runtime,
                            host,
                            device["redfish_port"],
                            (
                                self.config["redfish"]["username"],
                                self.config["redfish"]["password"],
                            ),
                            self._ssl_context(),
                        )
                    )
        except Exception:
            self.stop()
            raise
        for listener in self.ipmi_listeners:
            listener.start()
        for listener in self.snmp_listeners:
            listener.start()
        for listener in self.redfish_listeners:
            listener.start()
        self._started = True

    def stop(self) -> None:
        for listener in self.ipmi_listeners:
            listener.stop()
        for listener in self.snmp_listeners:
            listener.stop()
        for listener in self.redfish_listeners:
            try:
                listener.stop()
            except Exception:
                pass
        self.ipmi_listeners.clear()
        self.snmp_listeners.clear()
        self.redfish_listeners.clear()
        self._started = False

    def __enter__(self):
        self.start()
        return self

    def __exit__(self, exc_type, exc, traceback):
        self.stop()
        return False
