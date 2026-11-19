from __future__ import annotations

import argparse
import signal
import sys
import threading
from pathlib import Path

from .http_server import make_server
from .service import ArchiveService


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Local three-domain compliant archive service"
    )
    parser.add_argument(
        "--data-dir",
        type=Path,
        default=Path("archive-data"),
        help="database, staging and three domain directories (default: archive-data)",
    )
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8080)
    parser.add_argument("--bootstrap-regular-days", type=int, default=180)
    parser.add_argument("--bootstrap-audit-days", type=int, default=180)
    parser.add_argument("--bootstrap-legal-days", type=int, default=180)
    parser.add_argument(
        "--bootstrap-effective-at",
        default=None,
        help="optional ISO-8601 bootstrap policy effective time",
    )
    parser.add_argument(
        "--max-object-bytes",
        type=int,
        default=20 * 1024 * 1024,
        help="maximum request body accepted for one object version",
    )
    parser.add_argument("--quiet", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    service = ArchiveService(
        args.data_dir,
        bootstrap_regular_days=args.bootstrap_regular_days,
        bootstrap_audit_days=args.bootstrap_audit_days,
        bootstrap_legal_days=args.bootstrap_legal_days,
        bootstrap_effective_at=args.bootstrap_effective_at,
        max_object_bytes=args.max_object_bytes,
    )
    server = make_server(
        service,
        host=args.host,
        port=args.port,
        quiet=args.quiet,
    )
    host, port = server.server_address[:2]
    print(
        f"compliant-archive listening on http://{host}:{port} "
        f"(data-dir={service.data_dir})",
        flush=True,
    )

    def _stop(_signum: int, _frame: object) -> None:
        threading.Thread(target=server.shutdown, daemon=True).start()

    for name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, name):
            signal.signal(getattr(signal, name), _stop)
    try:
        server.serve_forever(poll_interval=0.2)
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
