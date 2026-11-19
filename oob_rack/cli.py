from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path

from .app import run_emulator, run_inspection, run_service
from .config import load_config
from .errors import OobError
from .generate import build_config

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_CONFIG = ROOT / "config" / "rack.json"


def _dump(value) -> None:
    print(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2))


def _http_json(method: str, url: str, body: dict | None = None) -> dict:
    data = None if body is None else json.dumps(body).encode("utf-8")
    request = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        request.add_header("Content-Type", "application/json")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(request, timeout=10) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        try:
            payload = json.loads(exc.read().decode("utf-8"))
        except Exception:
            payload = {"error": {"code": "E_HTTP", "message": f"HTTP {exc.code}"}}
        raise OobError(
            payload.get("error", {}).get("message", f"HTTP {exc.code}"),
            details=payload.get("error", {}),
        ) from exc
    except urllib.error.URLError as exc:
        raise OobError(f"HTTP endpoint is unavailable: {exc.reason}") from exc


def _config(args) -> dict:
    return load_config(args.config)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Lightweight IPMI/Redfish/SNMP rack simulator and compliance inspector."
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    generate = subparsers.add_parser("generate", help="generate the deterministic rack manifest")
    generate.add_argument("--output", default=str(DEFAULT_CONFIG))
    generate.add_argument("--seed", type=int, default=2872026)

    serve = subparsers.add_parser(
        "serve", aliases=["run"], help="run emulator, inspector, control and monitor"
    )
    serve.add_argument("--config", default=str(DEFAULT_CONFIG))
    serve.add_argument("--once", action="store_true", help="run one inspection and exit")
    serve.add_argument("--no-monitor", action="store_true")
    serve.add_argument("--json", action="store_true")

    emulator = subparsers.add_parser("emulator", help="run only the BMC emulator and fault control")
    emulator.add_argument("--config", default=str(DEFAULT_CONFIG))

    inspect = subparsers.add_parser("inspect", help="run one inspection against an existing emulator")
    inspect.add_argument("--config", default=str(DEFAULT_CONFIG))
    inspect.add_argument("--details", action="store_true")

    status = subparsers.add_parser("status", help="query a running monitor")
    status.add_argument("--config", default=str(DEFAULT_CONFIG))
    status.add_argument("--rack")

    faults = subparsers.add_parser("faults", help="manage emulator fault injection")
    faults.add_argument("--config", default=str(DEFAULT_CONFIG))
    fault_sub = faults.add_subparsers(dest="fault_command", required=True)
    fault_list = fault_sub.add_parser("list")
    fault_list.add_argument("--device")
    fault_add = fault_sub.add_parser("add")
    fault_add.add_argument("--kind", required=True)
    fault_add.add_argument("--device")
    fault_add.add_argument("--protocol")
    fault_add.add_argument("--sensor")
    fault_add.add_argument("--value")
    fault_clear = fault_sub.add_parser("clear")
    fault_clear.add_argument("--id", type=int)
    fault_clear.add_argument("--device")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "generate":
            output = Path(args.output)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(
                json.dumps(build_config(args.seed), ensure_ascii=False, indent=2, sort_keys=True)
                + "\n",
                encoding="utf-8",
            )
            print(str(output))
            return 0
        if args.command in ("serve", "run"):
            status = run_service(
                args.config, once=args.once, with_monitor=not args.no_monitor
            )
            if args.json or args.once:
                _dump(status)
            return 0
        if args.command == "emulator":
            run_emulator(args.config)
            return 0
        if args.command == "inspect":
            result = run_inspection(args.config)
            _dump(result["status"] if args.details else result)
            return 0
        if args.command == "status":
            config = _config(args)
            rack = args.rack or config["rack_id"]
            monitor = config["monitor"]
            url = f"http://{monitor['host']}:{monitor['port']}/status?rack={rack}"
            _dump(_http_json("GET", url))
            return 0
        if args.command == "faults":
            config = _config(args)
            control = config["control"]
            base = f"http://{control['host']}:{control['port']}"
            if args.fault_command == "list":
                suffix = f"?device_id={args.device}" if args.device else ""
                _dump(_http_json("GET", base + "/faults" + suffix))
                return 0
            if args.fault_command == "add":
                body = {"kind": args.kind}
                if args.device:
                    body["device_id"] = args.device
                if args.protocol:
                    body["protocol"] = args.protocol
                if args.sensor:
                    body["sensor"] = args.sensor
                if args.value is not None:
                    try:
                        body["value"] = int(args.value)
                    except ValueError:
                        try:
                            body["value"] = float(args.value)
                        except ValueError:
                            body["value"] = args.value
                _dump(_http_json("POST", base + "/faults", body))
                return 0
            if args.fault_command == "clear":
                params = []
                if args.id is not None:
                    params.append(f"id={args.id}")
                if args.device:
                    params.append(f"device_id={args.device}")
                suffix = "?" + "&".join(params) if params else ""
                _dump(_http_json("DELETE", base + "/faults" + suffix))
                return 0
        parser.error("unsupported command")
        return 2
    except OobError as exc:
        print(
            json.dumps(
                {"error": exc.as_dict()},
                ensure_ascii=False,
                sort_keys=True,
            ),
            file=sys.stderr,
        )
        return 2
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
