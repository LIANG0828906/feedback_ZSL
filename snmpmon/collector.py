"""Polling collector: SNMP GET/GETNEXT, retries, cooldown, deltas, alerts."""

import argparse
import json
import socket
import threading
import time

from . import ber, snmp
from .simulator import (IF_ADMIN_STATUS, IF_DESCR, IF_IN_OCTETS, IF_OPER_STATUS,
                        IF_OUT_OCTETS, SYS_UPTIME)
from .storage import Storage

COUNTER_MODULUS = 2 ** 32
WRAP_AMBIGUOUS_THRESHOLD = 2 ** 31


def oid_str(oid):
    return ".".join(str(a) for a in oid)


def _chunks(seq, size):
    for i in range(0, len(seq), size):
        yield seq[i:i + size]


class DevicePoller(threading.Thread):
    def __init__(self, device_cfg, cfg, storage, cycles):
        super().__init__(name=f"poller-{device_cfg['name']}", daemon=True)
        self.name = device_cfg["name"]
        self.addr = (device_cfg.get("host", "127.0.0.1"), int(device_cfg["port"]))
        self.community = device_cfg.get("community", "public")
        self.timeout = float(cfg.get("request_timeout_seconds", 0.5))
        self.retries = int(cfg.get("retries", 2))
        self.chunk_size = int(cfg.get("chunk_size", 32))
        self.interval = float(cfg.get("poll_interval_seconds", 0.05))
        self.dedup_window = int(cfg.get("dedup_window_cycles", 5))
        self.cooldown_threshold = int(cfg.get("failure_cooldown_threshold", 3))
        self.cooldown_cycles = int(cfg.get("cooldown_cycles", 5))
        self.cycles = cycles
        self.storage = storage
        self.request_id = 0
        self.timeouts = 0
        self.consecutive_failures = 0
        self.cooldown_left = 0
        self.indices = None
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.settimeout(self.timeout)
        self.start_cycle = storage.max_cycle(self.name) + 1
        self.prev_uptime = storage.device_uptime(self.name)
        self.prev_counters = storage.latest_counters(
            self.name, oid_str(IF_IN_OCTETS), oid_str(IF_OUT_OCTETS))
        self.if_state = storage.latest_oper_status(self.name, oid_str(IF_OPER_STATUS))
        self.open_alerts = storage.open_alerts(self.name)

    def request(self, pdu_tag, oids):
        for _ in range(self.retries + 1):
            self.request_id += 1
            payload = snmp.build_request(pdu_tag, self.request_id, oids, self.community)
            try:
                self.sock.sendto(payload, self.addr)
                while True:
                    data, _ = self.sock.recvfrom(65535)
                    resp = snmp.parse_message(data)
                    if resp["request_id"] != self.request_id:
                        continue
                    if resp["error_status"] != 0:
                        return None
                    return resp["varbinds"]
            except socket.timeout:
                self.timeouts += 1
            except (OSError, snmp.SnmpError, ValueError):
                return None
        return None

    def walk_column(self, column_oid):
        results = {}
        current = column_oid
        while True:
            varbinds = self.request(ber.PDU_GETNEXT, [current])
            if varbinds is None:
                return None
            oid, tag, raw = varbinds[0]
            if tag in ber.EXCEPTION_TAGS or oid[:len(column_oid)] != column_oid:
                return results
            results[oid[-1]] = snmp.decode_value(tag, raw)
            current = oid

    def run(self):
        try:
            for cycle in range(self.start_cycle, self.start_cycle + self.cycles):
                started = time.monotonic()
                if self.cooldown_left > 0:
                    self.cooldown_left -= 1
                    self.storage.write_cycle(
                        self.name, cycle, "skipped_cooldown",
                        "device in cooldown", 0, 0, [], [], [], [], None)
                else:
                    self.poll_cycle(cycle)
                elapsed = time.monotonic() - started
                if elapsed < self.interval:
                    time.sleep(self.interval - elapsed)
        finally:
            self.sock.close()

    def _record_failure(self, cycle, reason, samples, uptime_cs):
        self.consecutive_failures += 1
        if self.consecutive_failures >= self.cooldown_threshold:
            self.cooldown_left = self.cooldown_cycles
            self.consecutive_failures = 0
        self.storage.write_cycle(
            self.name, cycle, "failed", reason, 0, 0, samples, [], [],
            [("*", reason)], uptime_cs)

    def poll_cycle(self, cycle):
        timeouts_before = self.timeouts
        varbinds = self.request(ber.PDU_GET, [SYS_UPTIME])
        if varbinds is None:
            self._record_failure(cycle, "sysUpTime GET failed after retries", [], None)
            self._flush_timeouts(cycle, timeouts_before)
            return
        oid, tag, raw = varbinds[0]
        uptime = snmp.decode_value(tag, raw)
        samples = [(oid_str(SYS_UPTIME), uptime, str(uptime), "TimeTicks")]
        if self.indices is None:
            descr = self.walk_column(IF_DESCR)
            if descr is None:
                self._record_failure(cycle, "ifDescr walk failed", samples, uptime)
                self._flush_timeouts(cycle, timeouts_before)
                return
            self.indices = sorted(descr)
        oids = []
        for column in (IF_DESCR, IF_ADMIN_STATUS, IF_OPER_STATUS,
                       IF_IN_OCTETS, IF_OUT_OCTETS):
            for if_index in self.indices:
                oids.append(column + (if_index,))
        results = {}
        errors = []
        for chunk in _chunks(oids, self.chunk_size):
            varbinds = self.request(ber.PDU_GET, chunk)
            if varbinds is None:
                errors.extend((oid_str(o), "GET failed after retries") for o in chunk)
                continue
            for oid, tag, raw in varbinds:
                if tag in ber.EXCEPTION_TAGS:
                    errors.append((oid_str(oid), "agent returned exception"))
                    continue
                results[oid] = (snmp.decode_value(tag, raw), snmp.value_type_name(tag))
        for oid, (value, vtype) in results.items():
            samples.append((oid_str(oid), uptime, str(value), vtype))
        restart = self.prev_uptime is not None and uptime < self.prev_uptime[1]
        deltas = self._compute_deltas(results, uptime, restart)
        alert_events = self._evaluate_alerts(results, cycle)
        status = "partial" if errors else "ok"
        self.consecutive_failures = 0
        raised = self.storage.write_cycle(
            self.name, cycle, status, None, self.timeouts - timeouts_before,
            len(errors), samples, deltas, alert_events, errors, uptime)
        self.open_alerts.update(raised)
        for kind, if_index, alert_id in alert_events:
            if kind == "clear":
                self.open_alerts.pop(if_index, None)
            elif kind == "reopen":
                self.open_alerts[if_index] = alert_id
        self.prev_uptime = (cycle, uptime)

    def _flush_timeouts(self, cycle, timeouts_before):
        delta = self.timeouts - timeouts_before
        if delta:
            with self.storage.lock:
                with self.storage.conn:
                    self.storage.conn.execute(
                        "UPDATE device_cycles SET timeouts = ? "
                        "WHERE device = ? AND cycle = ?",
                        (delta, self.name, cycle))

    def _compute_deltas(self, results, uptime, restart):
        deltas = []
        for if_index in self.indices:
            for direction, column in (("in", IF_IN_OCTETS), ("out", IF_OUT_OCTETS)):
                entry = results.get(column + (if_index,))
                if entry is None:
                    continue
                current = int(entry[0])
                prev = self.prev_counters.get((if_index, direction))
                record = {"if_index": if_index, "direction": direction,
                          "delta": None, "elapsed_cs": None,
                          "status": "unavailable", "reason": None}
                if prev is None:
                    record["reason"] = "first_sample"
                elif restart:
                    record["reason"] = "device_restart"
                else:
                    prev_value, prev_ts = prev
                    elapsed = uptime - prev_ts if prev_ts is not None else 0
                    record["elapsed_cs"] = elapsed
                    if elapsed <= 0:
                        record["reason"] = "zero_interval"
                    else:
                        if current >= prev_value:
                            delta = current - prev_value
                        else:
                            delta = (COUNTER_MODULUS - prev_value) + current
                        if delta >= WRAP_AMBIGUOUS_THRESHOLD:
                            record["reason"] = "wrap_ambiguous"
                        else:
                            record["delta"] = delta
                            record["status"] = "ok"
                            if current < prev_value:
                                record["reason"] = "counter_wrap"
                deltas.append(record)
                self.prev_counters[(if_index, direction)] = (current, uptime)
        return deltas

    def _evaluate_alerts(self, results, cycle):
        events = []
        for if_index in self.indices:
            entry = results.get(IF_OPER_STATUS + (if_index,))
            if entry is None:
                continue
            oper = int(entry[0])
            prev = self.if_state.get(if_index)
            if prev == oper:
                continue
            if oper == 2 and prev == 1:
                last = self.storage.last_alert(self.name, if_index)
                if (last and last["cleared_cycle"] is not None
                        and cycle - last["cleared_cycle"] <= self.dedup_window):
                    events.append(("reopen", if_index, last["id"]))
                else:
                    events.append(("raise", if_index, None))
            elif oper == 1 and prev == 2:
                alert_id = self.open_alerts.get(if_index)
                if alert_id is not None:
                    events.append(("clear", if_index, alert_id))
            self.if_state[if_index] = oper
        return events


def build_report(storage, device_names):
    conn = storage.conn
    devices = {}
    totals = {"cycles_total": 0, "cycles_succeeded": 0, "cycles_failed": 0,
              "cycles_partial": 0, "cycles_skipped_cooldown": 0, "timeouts": 0,
              "oid_failures": 0, "alerts_raised": 0, "alerts_cleared": 0,
              "restart_unavailable_records": 0}
    for name in sorted(device_names):
        row = conn.execute(
            "SELECT COUNT(*), "
            "SUM(status = 'ok'), SUM(status = 'failed'), "
            "SUM(status = 'partial'), SUM(status = 'skipped_cooldown'), "
            "COALESCE(SUM(timeouts), 0), COALESCE(SUM(oid_failures), 0) "
            "FROM device_cycles WHERE device = ?", (name,)).fetchone()
        alerts_raised = conn.execute(
            "SELECT COUNT(*) FROM alert_events WHERE device = ? AND event = 'raise'",
            (name,)).fetchone()[0]
        alerts_cleared = conn.execute(
            "SELECT COUNT(*) FROM alert_events WHERE device = ? AND event = 'clear'",
            (name,)).fetchone()[0]
        restart_unavailable = conn.execute(
            "SELECT COUNT(*) FROM counter_deltas "
            "WHERE device = ? AND reason = 'device_restart'", (name,)).fetchone()[0]
        interfaces = {}
        rows = conn.execute(
            "SELECT oid, value FROM samples s WHERE device = ? "
            "AND cycle = (SELECT MAX(cycle) FROM samples "
            "             WHERE device = s.device AND oid = s.oid)",
            (name,)).fetchall()
        for oid, value in rows:
            parts = oid.split(".")
            column = int(parts[-2]) if len(parts) >= 2 else -1
            if column not in (8, 10, 16):
                continue
            if_index = int(parts[-1])
            key = f"if{if_index:02d}"
            entry = interfaces.setdefault(key, {})
            if column == 10:
                entry["ifInOctets"] = int(value)
            elif column == 16:
                entry["ifOutOctets"] = int(value)
            elif column == 8:
                entry["ifOperStatus"] = int(value)
        stats = {
            "cycles_total": int(row[0] or 0),
            "cycles_succeeded": int(row[1] or 0) + int(row[3] or 0),
            "cycles_failed": int(row[2] or 0),
            "cycles_partial": int(row[3] or 0),
            "cycles_skipped_cooldown": int(row[4] or 0),
            "timeouts": int(row[5] or 0),
            "oid_failures": int(row[6] or 0),
            "alerts_raised": int(alerts_raised),
            "alerts_cleared": int(alerts_cleared),
            "restart_unavailable_records": int(restart_unavailable),
            "interfaces": interfaces,
        }
        devices[name] = stats
        for key in totals:
            totals[key] += stats[key]
    return {"devices": devices, "totals": totals}


def render_report(report):
    return json.dumps(report, indent=2, sort_keys=True) + "\n"


def run_collectors(cfg, cycles, storage=None):
    own_storage = storage is None
    if own_storage:
        storage = Storage(cfg.get("database", "snmpmon.db"))
    pollers = [DevicePoller(dev, cfg, storage, cycles) for dev in cfg["devices"]]
    for poller in pollers:
        poller.start()
    for poller in pollers:
        poller.join()
    report = build_report(storage, [d["name"] for d in cfg["devices"]])
    if own_storage:
        storage.close()
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description="Run SNMP polling collectors.")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--cycles", type=int, default=200)
    args = parser.parse_args(argv)
    with open(args.config, "r", encoding="utf-8") as fh:
        cfg = json.load(fh)
    report = run_collectors(cfg, args.cycles)
    text = render_report(report)
    with open(cfg.get("report_file", "report.json"), "w", encoding="utf-8") as fh:
        fh.write(text)
    print(text, end="")


if __name__ == "__main__":
    main()
