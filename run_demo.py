"""One-command demo: start 3 simulated agents plus collectors, emit JSON report.

Usage: python run_demo.py --cycles 200
"""

import argparse
import json
import os
import shutil

from snmpmon.collector import build_report, render_report, DevicePoller
from snmpmon.simulator import SimulatedAgent
from snmpmon.storage import Storage


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--cycles", type=int, default=200)
    parser.add_argument("--keep", action="store_true",
                        help="keep existing database and simulator state")
    args = parser.parse_args()

    with open(args.config, "r", encoding="utf-8") as fh:
        cfg = json.load(fh)

    db_path = cfg.get("database", "snmpmon.db")
    state_dir = cfg.get("state_dir", "sim_state")
    report_file = cfg.get("report_file", "report.json")

    if not args.keep:
        for path in (db_path, db_path + "-wal", db_path + "-shm"):
            if os.path.exists(path):
                os.remove(path)
        if os.path.isdir(state_dir):
            shutil.rmtree(state_dir)
    os.makedirs(state_dir, exist_ok=True)

    agents = [SimulatedAgent(dev, cfg.get("seed", 1), state_dir)
              for dev in cfg["devices"]]
    for agent in agents:
        agent.start()
    for agent in agents:
        agent.bound.wait()

    storage = Storage(db_path)
    try:
        pollers = [DevicePoller(dev, cfg, storage, args.cycles)
                   for dev in cfg["devices"]]
        for poller in pollers:
            poller.start()
        for poller in pollers:
            poller.join()
        report = build_report(storage, [d["name"] for d in cfg["devices"]])
    finally:
        storage.close()
        for agent in agents:
            agent.stop()
        for agent in agents:
            agent.join()

    text = render_report(report)
    with open(report_file, "w", encoding="utf-8") as fh:
        fh.write(text)
    print(text, end="")


if __name__ == "__main__":
    main()
