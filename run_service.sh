#!/usr/bin/env sh
set -eu
cd "$(dirname "$0")"
if [ ! -f metadata.db ]; then
  python3 generate_metadata.py --db metadata.db
fi
exec python3 lineage_service.py --db metadata.db --host 127.0.0.1 --port 8008
