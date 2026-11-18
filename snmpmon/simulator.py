"""Deterministic SNMP agent simulator.

Each agent listens on 127.0.0.1:<port> over UDP and serves a small MIB-2
subtree (sysUpTime + ifDescr/ifAdminStatus/ifOperStatus/ifInOctets/
ifOutOctets). Counters are a pure function of a logical tick that advances
once per processed sysUpTime GET, so values are deterministic for a given
seed and independent of wall-clock time. The tick is persisted to a JSON
state file so counters stay continuous across simulator restarts.
"""

import argparse
import json
import os
import random
import socket
import threading
import time

from . import ber, snmp

SYS_UPTIME = (1, 3, 6, 1, 2, 1, 1, 3, 0)
IF_MIB = (1, 3, 6, 1, 2, 1, 2, 2, 1)
IF_DESCR = IF_MIB + (2,)
IF_ADMIN_STATUS = IF_MIB + (7,)
IF_OPER_STATUS = IF_MIB + (8,)
IF_IN_OCTETS = IF_MIB + (10,)
IF_OUT_OCTETS = IF_MIB + (16,)

COUNTER_MODULUS = 2 ** 32


def _int_raw(value):
    length = 1
    while not (-(1 << (8 * length - 1)) <= value < (1 << (8 * length - 1))):
        length += 1
    return value.to_bytes(length, "big", signed=True)


def _unsigned_raw(value):
    raw = value.to_bytes(max(1, (value.bit_length() + 7) // 8), "big")
    if raw[0] & 0x80:
        raw = b"\x00" + raw
    return raw


class SimulatedAgent(threading.Thread):
    def __init__(self, device_cfg, seed, state_dir):
        super().__init__(name=f"sim-{device_cfg['name']}", daemon=True)
        self.cfg = device_cfg
        self.name = device_cfg["name"]
        self.bind_addr = (device_cfg.get("host", "127.0.0.1"), int(device_cfg["port"]))
        self.if_count = int(device_cfg.get("interfaces", 16))
        self.latency = float(device_cfg.get("latency_ms", 0)) / 1000.0
        self.loss_percent = float(device_cfg.get("loss_percent", 0.0))
        self.flaps = device_cfg.get("flaps", [])
        self.restart_ticks = sorted(device_cfg.get("restart_at_ticks", []))
        self.dead_windows = device_cfg.get("dead_request_windows", [])
        self.admin_down = set(device_cfg.get("admin_down_interfaces", []))

        params = random.Random(f"{seed}:{self.name}:params")
        self.base = {i: params.getrandbits(32) for i in range(1, self.if_count + 1)}
        self.rate = {i: params.randint(1_000, 5_000_000) for i in range(1, self.if_count + 1)}
        for i in device_cfg.get("high_rate_interfaces", []):
            self.rate[int(i)] = params.randint(COUNTER_MODULUS // 45, COUNTER_MODULUS // 30)
        self.loss_rng = random.Random(f"{seed}:{self.name}:loss")

        self.state_path = os.path.join(state_dir, f"{self.name}.json")
        self.tick = 0
        self.restarts_done = 0
        self.requests = 0
        self._load_state()

        self._stop_event = threading.Event()
        self.bound = threading.Event()
        self.sock = None

    def _load_state(self):
        try:
            with open(self.state_path, "r", encoding="utf-8") as fh:
                state = json.load(fh)
            self.tick = int(state.get("tick", 0))
            self.restarts_done = int(state.get("restarts_done", 0))
            self.requests = int(state.get("requests", 0))
        except (OSError, ValueError):
            pass

    def _save_state(self):
        tmp = self.state_path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump(
                {"tick": self.tick, "restarts_done": self.restarts_done,
                 "requests": self.requests},
                fh, sort_keys=True,
            )
        os.replace(tmp, self.state_path)

    def _advance_tick(self):
        self.tick += 1
        if (self.restarts_done < len(self.restart_ticks)
                and self.tick == self.restart_ticks[self.restarts_done]):
            self.tick = 0
            self.restarts_done += 1
        self._save_state()

    def _oper_status(self, if_index):
        status = 1
        for flap in self.flaps:
            if (int(flap["if_index"]) == if_index
                    and int(flap["down_tick"]) <= self.tick < int(flap["up_tick"])):
                status = 2
        if if_index in self.admin_down:
            status = 2
        return status

    def _counter(self, if_index):
        base = 0 if self.restarts_done > 0 else self.base[if_index]
        return (base + self.rate[if_index] * self.tick) % COUNTER_MODULUS

    def _build_table(self):
        table = {SYS_UPTIME: (ber.TAG_TIMETICKS, _unsigned_raw(self.tick * 100))}
        for i in range(1, self.if_count + 1):
            table[IF_DESCR + (i,)] = (
                ber.TAG_OCTET_STRING, f"GigabitEthernet1/0/{i}".encode("utf-8"))
            table[IF_ADMIN_STATUS + (i,)] = (
                ber.TAG_INTEGER, _int_raw(2 if i in self.admin_down else 1))
            table[IF_OPER_STATUS + (i,)] = (
                ber.TAG_INTEGER, _int_raw(self._oper_status(i)))
            table[IF_IN_OCTETS + (i,)] = (
                ber.TAG_COUNTER32, _unsigned_raw(self._counter(i)))
            out_counter = (self._counter(i) * 7 + 13) % COUNTER_MODULUS
            table[IF_OUT_OCTETS + (i,)] = (
                ber.TAG_COUNTER32, _unsigned_raw(out_counter))
        return table

    def _handle(self, data):
        try:
            msg = snmp.parse_message(data)
        except Exception:
            return None
        if msg["pdu"] not in (ber.PDU_GET, ber.PDU_GETNEXT):
            return None
        requested = [oid for oid, _, _ in msg["varbinds"]]
        if msg["pdu"] == ber.PDU_GET and SYS_UPTIME in requested:
            self._advance_tick()
        table = self._build_table()
        ordered = sorted(table)
        out = []
        for oid in requested:
            if msg["pdu"] == ber.PDU_GET:
                if oid in table:
                    out.append((oid, table[oid][0], table[oid][1]))
                else:
                    out.append((oid, ber.TAG_NO_SUCH_OBJECT, b""))
            else:
                nxt = None
                for candidate in ordered:
                    if candidate > oid:
                        nxt = candidate
                        break
                if nxt is None:
                    out.append((oid, ber.TAG_END_OF_MIB_VIEW, b""))
                else:
                    out.append((nxt, table[nxt][0], table[nxt][1]))
        return snmp.build_response(msg["request_id"], out, msg["community"], msg["version"])

    def run(self):
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.sock.bind(self.bind_addr)
        self.sock.settimeout(0.2)
        self.bound.set()
        while not self._stop_event.is_set():
            try:
                data, addr = self.sock.recvfrom(65535)
            except socket.timeout:
                continue
            except OSError:
                break
            self.requests += 1
            if any(int(s) <= self.requests <= int(e) for s, e in self.dead_windows):
                continue
            if self.loss_percent > 0 and self.loss_rng.random() * 100.0 < self.loss_percent:
                continue
            if self.latency > 0:
                time.sleep(self.latency)
            response = self._handle(data)
            if response is not None:
                try:
                    self.sock.sendto(response, addr)
                except OSError:
                    break
        try:
            self.sock.close()
        except OSError:
            pass

    def stop(self):
        self._stop_event.set()


def main(argv=None):
    parser = argparse.ArgumentParser(description="Run SNMP agent simulators.")
    parser.add_argument("--config", default="config.json")
    args = parser.parse_args(argv)
    with open(args.config, "r", encoding="utf-8") as fh:
        cfg = json.load(fh)
    os.makedirs(cfg.get("state_dir", "sim_state"), exist_ok=True)
    agents = [
        SimulatedAgent(dev, cfg.get("seed", 1), cfg.get("state_dir", "sim_state"))
        for dev in cfg["devices"]
    ]
    for agent in agents:
        agent.start()
    for agent in agents:
        agent.bound.wait()
    print(f"started {len(agents)} simulated agents", flush=True)
    try:
        while True:
            time.sleep(1)
    except KeyboardInterrupt:
        pass
    finally:
        for agent in agents:
            agent.stop()
        for agent in agents:
            agent.join()


if __name__ == "__main__":
    main()
