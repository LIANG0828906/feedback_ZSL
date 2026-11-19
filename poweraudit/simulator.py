"""Read-only local SNMP v2c device simulator.

The simulator speaks standard SNMP v2c over UDP and is deliberately
stateless from the poller's perspective: GET/GETNEXT only read an immutable
value map. It can also be used when pysnmp is unavailable, while the OID
layout remains compatible with pysnmp-based agents and clients.
"""

from __future__ import annotations

import bisect
import logging
import selectors
import socket
import threading
from dataclasses import dataclass
from typing import Any, Iterable

from .ber import (
    TAG_END_OF_MIB_VIEW,
    TAG_GET_BULK_REQUEST,
    TAG_GET_NEXT_REQUEST,
    TAG_GET_REQUEST,
    TAG_GET_RESPONSE,
    TAG_INTEGER,
    TAG_NO_SUCH_OBJECT,
    TAG_OCTET_STRING,
    TAG_SET_REQUEST,
    BerError,
    Varbind,
    decode_message,
    encode_message,
    oid_to_tuple,
)
from .constants import (
    DEVICE_ID,
    DEVICE_MODEL,
    DEVICE_STATUS,
    DEVICE_STATUS_ONLINE,
    DEVICE_TYPE,
    PDU_PORT_BRANCH,
    PDU_PORT_SERVER,
    PDU_PORT_STATUS,
    PDU_PORT_UPS,
    PDU_UPLINK_BRANCH,
    PDU_UPLINK_UPS,
    PORT_STATUS_UP,
    SUPPLY_STATE_MAINS,
    SYS_DESCR,
    SYS_OBJECT_ID,
    UPS_BATTERY_REMAINING,
    UPS_OUTPUT_LOAD,
    UPS_RUNTIME_MINUTES,
    UPS_SUPPLY_STATE,
)
from .inventory import Inventory

LOG = logging.getLogger(__name__)


def _status_value(value: Any) -> int:
    if isinstance(value, int) and not isinstance(value, bool):
        return value
    names = {
        "online": 1,
        "on_battery": 2,
        "warning": 3,
        "offline": 4,
    }
    return names.get(str(value).lower(), DEVICE_STATUS_ONLINE)


def _supply_value(value: Any) -> int:
    if isinstance(value, int) and not isinstance(value, bool):
        return value
    return 2 if str(value).lower() in ("battery", "on_battery") else SUPPLY_STATE_MAINS


def _port_status(value: Any) -> int:
    if isinstance(value, int) and not isinstance(value, bool):
        return value
    return 0 if str(value).lower() in ("down", "off", "0", "false") else PORT_STATUS_UP


@dataclass(frozen=True)
class SimulatedDevice:
    device_id: str
    device_type: str
    host: str
    port: int
    community: str
    values: dict[tuple[int, ...], tuple[int, Any]]


class MibAgent:
    def __init__(self, values: dict[tuple[int, ...], tuple[int, Any]], community: str) -> None:
        self.values = dict(values)
        self.community = community
        self.sorted_oids = tuple(sorted(self.values))

    def _next_oid(self, oid: tuple[int, ...]) -> tuple[int, ...] | None:
        index = bisect.bisect_right(self.sorted_oids, oid)
        if index >= len(self.sorted_oids):
            return None
        return self.sorted_oids[index]

    def _response_value(self, varbind: Varbind) -> Varbind:
        found = self.values.get(varbind.oid)
        if found is None:
            return Varbind(varbind.oid, TAG_NO_SUCH_OBJECT, None)
        tag, value = found
        return Varbind(varbind.oid, tag, value)

    def _next_value(self, varbind: Varbind) -> Varbind:
        next_oid = self._next_oid(varbind.oid)
        if next_oid is None:
            return Varbind(varbind.oid, TAG_END_OF_MIB_VIEW, None)
        tag, value = self.values[next_oid]
        return Varbind(next_oid, tag, value)

    def handle(self, packet: bytes, address: tuple[str, int]) -> bytes | None:
        try:
            request = decode_message(packet)
        except (BerError, ValueError):
            return None
        if request.community != self.community:
            return None
        if request.pdu_tag == TAG_GET_REQUEST:
            response_vbs = [self._response_value(vb) for vb in request.varbinds]
            error_status = 0
            error_index = 0
        elif request.pdu_tag == TAG_GET_NEXT_REQUEST:
            response_vbs = [self._next_value(vb) for vb in request.varbinds]
            error_status = 0
            error_index = 0
        elif request.pdu_tag == TAG_GET_BULK_REQUEST:
            non_repeaters = max(0, request.error_status)
            max_repetitions = max(0, request.error_index)
            response_vbs = []
            for index, vb in enumerate(request.varbinds):
                if index < non_repeaters:
                    response_vbs.append(self._next_value(vb))
                    continue
                current = vb
                for _ in range(max_repetitions):
                    current = self._next_value(current)
                    response_vbs.append(current)
                    if current.tag == TAG_END_OF_MIB_VIEW:
                        break
            error_status = 0
            error_index = 0
        elif request.pdu_tag == TAG_SET_REQUEST:
            response_vbs = list(request.varbinds)
            error_status = 17  # notWritable
            error_index = 1 if response_vbs else 0
        else:
            return None
        try:
            return encode_message(
                self.community,
                TAG_GET_RESPONSE,
                request.request_id,
                (),
                error_status=error_status,
                error_index=error_index,
                varbinds=response_vbs,
            )
        except (BerError, ValueError):
            return None


def _merge_simulation(device: dict[str, Any], scenario_device: dict[str, Any]) -> dict[str, Any]:
    merged: dict[str, Any] = {}
    inventory_simulation = device.get("simulation", {})
    if isinstance(inventory_simulation, dict):
        merged.update(inventory_simulation)
    if isinstance(scenario_device, dict):
        merged.update(scenario_device)
    return merged


def _derive_pdu_ports(
    device: dict[str, Any],
    simulation: dict[str, Any],
    inventory: Inventory,
) -> dict[str, dict[str, Any]]:
    configured_ports = simulation.get("ports", {})
    ports: dict[str, dict[str, Any]] = {}
    if isinstance(configured_ports, dict):
        for raw_port, raw_data in configured_ports.items():
            try:
                port = int(raw_port)
            except (TypeError, ValueError):
                continue
            if isinstance(raw_data, dict):
                ports[str(port)] = dict(raw_data)
        # An explicit port table is authoritative. Never infer missing
        # connections from the asset declarations, otherwise stale inventory
        # entries would incorrectly become observed topology.
        if "ports" in simulation:
            return ports
    for server_id in inventory.server_order:
        for power_input in inventory.servers[server_id]["power_inputs"]:
            if power_input["pdu_id"] != device["device_id"]:
                continue
            key = str(power_input["port"])
            if key in ports:
                continue
            ports[key] = {
                "server_id": server_id,
                "status": "up",
                "upstream_ups_id": power_input["declared_ups_id"],
                "upstream_branch_id": power_input.get("declared_upstream_branch_id"),
            }
    return ports


def _build_values(
    device: dict[str, Any],
    simulation: dict[str, Any],
    inventory: Inventory,
) -> dict[tuple[int, ...], tuple[int, Any]]:
    values: dict[tuple[int, ...], tuple[int, Any]] = {}
    device_id = device["device_id"]
    device_type = device["device_type"]
    status = _status_value(simulation.get("status", "online"))

    def put(oid: str, tag: int, value: Any) -> None:
        values[oid_to_tuple(oid)] = (tag, value)

    put(SYS_DESCR, TAG_OCTET_STRING, f"PowerAudit simulated {device_type.upper()} {device_id}")
    put(SYS_OBJECT_ID, TAG_OCTET_STRING, ".1.3.6.1.4.1.424242.1")
    put(DEVICE_ID, TAG_OCTET_STRING, device_id)
    put(DEVICE_TYPE, TAG_OCTET_STRING, device_type.upper())
    put(DEVICE_STATUS, TAG_INTEGER, status)
    put(DEVICE_MODEL, TAG_OCTET_STRING, simulation.get("model", f"SIM-{device_type.upper()}"))

    if device_type == "pdu":
        ports = _derive_pdu_ports(device, simulation, inventory)
        first_port = next(iter(sorted(ports.values(), key=lambda item: str(item))), {})
        uplink_ups = simulation.get("uplink_ups_id") or first_port.get("upstream_ups_id") or ""
        uplink_branch = (
            simulation.get("uplink_branch_id")
            or first_port.get("upstream_branch_id")
            or ""
        )
        put(PDU_UPLINK_UPS, TAG_OCTET_STRING, str(uplink_ups))
        put(PDU_UPLINK_BRANCH, TAG_OCTET_STRING, str(uplink_branch))
        for raw_port, port_data in ports.items():
            port = int(raw_port)
            if not (1 <= port <= device["port_count"]):
                continue
            port_ups = port_data.get("upstream_ups_id") or uplink_ups
            port_branch = port_data.get("upstream_branch_id") or uplink_branch
            put(f"{PDU_PORT_UPS}.{port}", TAG_OCTET_STRING, str(port_ups))
            put(f"{PDU_PORT_BRANCH}.{port}", TAG_OCTET_STRING, str(port_branch))
            put(f"{PDU_PORT_STATUS}.{port}", TAG_INTEGER, _port_status(port_data.get("status")))
            put(
                f"{PDU_PORT_SERVER}.{port}",
                TAG_OCTET_STRING,
                str(port_data.get("server_id") or ""),
            )
        # Explicitly expose every configured port as disconnected when the
        # scenario does not assign it a server.
        for port in range(1, device["port_count"] + 1):
            if oid_to_tuple(f"{PDU_PORT_STATUS}.{port}") not in values:
                put(f"{PDU_PORT_UPS}.{port}", TAG_OCTET_STRING, str(uplink_ups))
                put(f"{PDU_PORT_BRANCH}.{port}", TAG_OCTET_STRING, str(uplink_branch))
                put(f"{PDU_PORT_STATUS}.{port}", TAG_INTEGER, PORT_STATUS_UP)
                put(f"{PDU_PORT_SERVER}.{port}", TAG_OCTET_STRING, "")
    else:
        load = simulation.get("output_load_percent", 0)
        battery = simulation.get("battery_remaining_percent", 100)
        runtime = simulation.get("runtime_minutes", 60)
        supply = _supply_value(simulation.get("supply_state", "mains"))
        put(UPS_OUTPUT_LOAD, TAG_INTEGER, int(load))
        put(UPS_BATTERY_REMAINING, TAG_INTEGER, int(battery))
        put(UPS_RUNTIME_MINUTES, TAG_INTEGER, int(runtime))
        put(UPS_SUPPLY_STATE, TAG_INTEGER, supply)
    return values


def build_simulated_devices(
    inventory: Inventory,
    scenario: dict[str, Any] | None = None,
    *,
    enabled_device_ids: set[str] | None = None,
) -> list[SimulatedDevice]:
    scenario_devices = (scenario or {}).get("devices", {})
    if not isinstance(scenario_devices, dict):
        scenario_devices = {}
    devices: list[SimulatedDevice] = []
    for device_id in inventory.device_order:
        device = inventory.devices[device_id]
        scenario_device = scenario_devices.get(device_id, {})
        simulation = _merge_simulation(device, scenario_device)
        enabled = bool(simulation.get("enabled", device.get("enabled", True)))
        if enabled_device_ids is not None:
            enabled = enabled and device_id in enabled_device_ids
        if not enabled:
            continue
        values = _build_values(device, simulation, inventory)
        devices.append(
            SimulatedDevice(
                device_id=device_id,
                device_type=device["device_type"],
                host=device["host"],
                port=device["port"],
                community=device["community"],
                values=values,
            )
        )
    return devices


class SnmpAgentServer:
    def __init__(self, devices: Iterable[SimulatedDevice]) -> None:
        self.devices = list(devices)
        self.selector = selectors.DefaultSelector()
        self.agents: dict[socket.socket, MibAgent] = {}
        self.stop_event = threading.Event()
        self.started = False
        self.thread: threading.Thread | None = None

    def start(self) -> None:
        if self.started:
            return
        try:
            for device in self.devices:
                family = socket.AF_INET6 if ":" in device.host else socket.AF_INET
                sock = socket.socket(family, socket.SOCK_DGRAM)
                try:
                    sock.bind((device.host, device.port))
                except OSError:
                    sock.close()
                    raise
                sock.setblocking(False)
                self.selector.register(sock, selectors.EVENT_READ)
                self.agents[sock] = MibAgent(device.values, device.community)
            self.started = True
        except Exception:
            self.close()
            raise

    def serve_forever(self, poll_interval: float = 0.25) -> None:
        self.start()
        while not self.stop_event.is_set():
            for key, _mask in self.selector.select(poll_interval):
                sock = key.fileobj
                agent = self.agents[sock]
                try:
                    packet, address = sock.recvfrom(65535)
                except (BlockingIOError, InterruptedError):
                    continue
                except OSError:
                    continue
                response = agent.handle(packet, address)
                if response is not None:
                    try:
                        sock.sendto(response, address)
                    except OSError:
                        continue

    def start_thread(self) -> None:
        self.start()
        self.thread = threading.Thread(target=self.serve_forever, name="snmp-simulator", daemon=True)
        self.thread.start()

    def stop(self) -> None:
        self.stop_event.set()
        if self.thread and self.thread.is_alive():
            self.thread.join(timeout=2.0)
        self.close()

    def close(self) -> None:
        for sock in list(self.agents):
            try:
                self.selector.unregister(sock)
            except Exception:
                pass
            try:
                sock.close()
            except OSError:
                pass
        self.agents.clear()
        try:
            self.selector.close()
        except Exception:
            pass
        self.started = False

    def __enter__(self) -> "SnmpAgentServer":
        self.start()
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.close()


def build_agent_server(
    devices: Iterable[SimulatedDevice],
    *,
    backend: str = "auto",
) -> Any:
    """Build a pysnmp-backed agent when available, with a protocol fallback."""
    normalized = (backend or "auto").strip().lower()
    if normalized not in ("auto", "pysnmp", "builtin"):
        raise ValueError("simulator backend must be auto, pysnmp, or builtin")
    device_list = list(devices)
    if not device_list:
        return SnmpAgentServer(device_list)
    if normalized in ("auto", "pysnmp"):
        try:
            from .pysnmp_backend import PysnmpAgentServer

            return PysnmpAgentServer(device_list)
        except Exception as exc:
            if normalized == "pysnmp":
                raise
            LOG.info("pysnmp backend unavailable (%s); using built-in backend", exc)
    return SnmpAgentServer(device_list)
