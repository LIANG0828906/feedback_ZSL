"""Optional pysnmp 4.4.12 command-responder backend.

One pysnmp engine hosts all simulated UDP endpoints. Each endpoint has its
own read-only MIB instrumentation and is selected by the incoming transport
domain, which keeps the footprint low instead of starting one process per
device.
"""

from __future__ import annotations

import importlib.machinery  # noqa: F401 - pysnmp 4.4.x probes importlib.util
import importlib.util  # noqa: F401
import threading
from typing import Any, Iterable

from .simulator import SimulatedDevice


class PysnmpUnavailable(RuntimeError):
    pass


def _load_pysnmp() -> dict[str, Any]:
    try:
        from pysnmp.carrier.asyncore.dgram import udp
        from pysnmp.entity import config, engine
        from pysnmp.entity.rfc3413 import cmdrsp, context
        from pysnmp.proto import rfc1902
        from pysnmp.smi import builder, instrum
    except Exception as exc:  # ImportError or an incompatible Python runtime.
        raise PysnmpUnavailable(f"pysnmp backend unavailable: {exc}") from exc
    return {
        "udp": udp,
        "config": config,
        "engine": engine,
        "cmdrsp": cmdrsp,
        "context": context,
        "rfc1902": rfc1902,
        "builder": builder,
        "instrum": instrum,
    }


def _asn1_value(rfc1902: Any, tag: int, value: Any) -> Any:
    if tag == 0x02:
        return rfc1902.Integer(int(value))
    if tag == 0x04:
        return rfc1902.OctetString(str(value))
    if tag == 0x06:
        return rfc1902.ObjectIdentifier(tuple(value))
    if tag == 0x41:
        return rfc1902.Counter32(int(value))
    if tag == 0x42:
        return rfc1902.Gauge32(int(value))
    if tag == 0x43:
        return rfc1902.TimeTicks(int(value))
    if tag == 0x46:
        return rfc1902.Counter64(int(value))
    return rfc1902.OctetString(str(value))


def _build_instrument(device: SimulatedDevice, modules: dict[str, Any]) -> Any:
    builder = modules["builder"]
    rfc1902 = modules["rfc1902"]
    mib_builder = builder.MibBuilder()
    mib_scalar, mib_scalar_instance = mib_builder.importSymbols(
        "SNMPv2-SMI", "MibScalar", "MibScalarInstance"
    )
    exports: dict[str, Any] = {}
    for index, oid in enumerate(sorted(device.values)):
        tag, value = device.values[oid]
        syntax = _asn1_value(rfc1902, tag, value)
        base = oid[:-1]
        exports[f"s{index}"] = mib_scalar(base, syntax.clone()).setMaxAccess(
            "readonly"
        )
        exports[f"i{index}"] = mib_scalar_instance(base, oid[-1:], syntax)
    mib_builder.exportSymbols("__POWER_AUDIT_SIMULATION__", **exports)
    return modules["instrum"].MibInstrumController(mib_builder)


class PysnmpAgentServer:
    """A single-engine, multi-endpoint pysnmp command responder."""

    def __init__(self, devices: Iterable[SimulatedDevice]) -> None:
        self.devices = list(devices)
        self._modules = _load_pysnmp()
        self._engine = None
        self._thread: threading.Thread | None = None
        self._started = False
        self._stopped = False

    @property
    def agents(self) -> list[SimulatedDevice]:
        return self.devices

    def start(self) -> None:
        if self._started:
            return
        modules = self._modules
        udp = modules["udp"]
        config = modules["config"]
        engine = modules["engine"]
        context = modules["context"]
        cmdrsp = modules["cmdrsp"]
        rfc1902 = modules["rfc1902"]
        instrum = modules["instrum"]

        snmp_engine = engine.SnmpEngine()
        mapping: dict[tuple[int, ...], Any] = {}

        class TransportMibInstrum(instrum.AbstractMibInstrumController):
            def _instrument(self, ac_info: Any) -> Any:
                ac_context = ac_info[1]
                execution = ac_context.observer.getExecutionContext(
                    "rfc3412.receiveMessage:request"
                )
                return mapping[execution["transportDomain"]]

            def readVars(self, var_binds: Any, ac_info: Any = (None, None)) -> Any:
                return self._instrument(ac_info).readVars(var_binds, ac_info)

            def readNextVars(
                self, var_binds: Any, ac_info: Any = (None, None)
            ) -> Any:
                return self._instrument(ac_info).readNextVars(var_binds, ac_info)

            def writeVars(self, var_binds: Any, ac_info: Any = (None, None)) -> Any:
                return self._instrument(ac_info).writeVars(var_binds, ac_info)

        try:
            communities: set[str] = set()
            for index, device in enumerate(self.devices):
                domain = udp.domainName + (1000 + index,)
                mapping[domain] = _build_instrument(device, modules)
                config.addTransport(
                    snmp_engine,
                    domain,
                    udp.UdpTransport().openServerMode((device.host, device.port)),
                )
                communities.add(device.community)
            context_name = "poweraudit-simulation"
            for index, community in enumerate(sorted(communities)):
                security_name = f"poweraudit-area-{index}"
                config.addV1System(
                    snmp_engine,
                    security_name,
                    community,
                    contextName=context_name,
                )
                config.addVacmUser(
                    snmp_engine,
                    2,
                    security_name,
                    "noAuthNoPriv",
                    (1, 3, 6),
                    (1, 3, 6),
                    contextName=context_name,
                )
            snmp_context = context.SnmpContext(snmp_engine)
            snmp_context.registerContextName(
                rfc1902.OctetString(context_name), TransportMibInstrum()
            )
            cmdrsp.GetCommandResponder(snmp_engine, snmp_context)
            cmdrsp.NextCommandResponder(snmp_engine, snmp_context)
            cmdrsp.BulkCommandResponder(snmp_engine, snmp_context)
            snmp_engine.transportDispatcher.jobStarted(1)
        except Exception:
            try:
                snmp_engine.transportDispatcher.closeDispatcher()
            except Exception:
                pass
            raise
        self._engine = snmp_engine
        self._started = True

    def serve_forever(self, poll_interval: float = 0.25) -> None:
        self.start()
        self._engine.transportDispatcher.runDispatcher()

    def start_thread(self) -> None:
        self.start()
        self._thread = threading.Thread(
            target=self._engine.transportDispatcher.runDispatcher,
            name="pysnmp-simulator",
            daemon=True,
        )
        self._thread.start()

    def stop(self) -> None:
        if self._stopped:
            return
        self._stopped = True
        if self._engine is not None and self._started:
            try:
                self._engine.transportDispatcher.jobFinished(1)
            except Exception:
                pass
            try:
                self._engine.transportDispatcher.closeDispatcher()
            except Exception:
                pass
        if self._thread is not None and self._thread.is_alive():
            self._thread.join(timeout=2.0)

    def close(self) -> None:
        self.stop()

    def __enter__(self) -> "PysnmpAgentServer":
        self.start()
        return self

    def __exit__(self, exc_type: Any, exc: Any, tb: Any) -> None:
        self.stop()
