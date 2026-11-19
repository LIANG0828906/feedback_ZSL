#!/usr/bin/env python3
"""slotguard 命令行入口。"""
from __future__ import annotations

import argparse
import json
import logging
import sys

from config import load_config
from metrics import start_metrics_server
from resync import ResyncManager
from service import SlotGuardService
from store import Store


def print_json(payload) -> None:
    print(json.dumps(payload, ensure_ascii=False, indent=2, default=str))


def build(argv=None):
    parser = argparse.ArgumentParser(description="复制槽保护与订阅重引导服务")
    parser.add_argument("--config", help="config.json 路径")
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("run", help="常驻：监控+抽样+位点核对+指标服务")
    sub.add_parser("monitor", help="执行一次槽状态汇总与释压判定")
    sub.add_parser("slots", help="打印当前统一槽视图")
    p_resync = sub.add_parser("resync", help="启动/继续一致性重同步")
    p_resync.add_argument("--reason", default="manual")
    sub.add_parser("resync-progress", help="查看两表重同步进度")
    sub.add_parser("sample", help="执行一次只读一致性抽样")
    sub.add_parser("reconcile", help="核对并修正 Redis 消费位点")
    sub.add_parser("advices", help="列出待处理释压建议")
    sub.add_parser("metrics", help="打印一次 Prometheus 指标文本")
    sub.add_parser("verify", help="校验主备行数与孤儿明细")
    return parser.parse_args(argv)


def main(argv=None) -> int:
    args = build(argv)
    cfg = load_config(args.config)
    store = Store(cfg.sqlite_path)
    service = SlotGuardService(cfg, store)
    try:
        if args.cmd == "run":
            logging.basicConfig(
                level=logging.INFO,
                format="%(asctime)s %(levelname)s %(name)s %(message)s",
            )
            server = start_metrics_server(cfg, store)
            print(f"metrics on http://{cfg.metrics_host}:{cfg.metrics_port}/metrics")
            try:
                service.run_forever()
            finally:
                server.shutdown()
        elif args.cmd == "monitor":
            print_json(service.monitor_once())
        elif args.cmd == "slots":
            import slots as slots_mod
            print_json(slots_mod.collect(cfg))
        elif args.cmd == "resync":
            print_json(service.resync.run(args.reason))
        elif args.cmd == "resync-progress":
            print_json(service.resync.progress())
        elif args.cmd == "sample":
            print_json(service.sampling_once())
        elif args.cmd == "reconcile":
            print_json(service.reconcile_once())
        elif args.cmd == "advices":
            print_json([dict(r) for r in store.list_pending_advices()])
        elif args.cmd == "metrics":
            from metrics import render_metrics
            print(render_metrics(cfg, store), end="")
        elif args.cmd == "verify":
            print_json(ResyncManager(cfg, store).verify())
        return 0
    finally:
        service.close()
        store.close()


if __name__ == "__main__":
    sys.exit(main())
