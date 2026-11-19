#!/usr/bin/env bash
# Reuse a running SQL Server if TCP 1433 is already served. Otherwise start a
# bounded local SQL Server 2022 Developer container for validation.
set -euo pipefail

CONTAINER_NAME="${OG_CONTAINER_NAME:-ordercutover-sql2022}"
IMAGE="${OG_SQL_IMAGE:-mcr.microsoft.com/mssql/server:2022-latest}"
SA_PASSWORD="${OG_SA_PASSWORD:-${SA_PASSWORD:-}}"
HOST_PORT="${OG_SQL_PORT:-1433}"

if [ -z "$SA_PASSWORD" ] && [ -z "${OG_CONNECTION_STRING:-}" ]; then
  echo "[sql] OG_SA_PASSWORD is required to start or probe the local instance" >&2
  exit 1
fi

if timeout 2 bash -c "echo > /dev/tcp/127.0.0.1/$HOST_PORT" 2>/dev/null; then
  echo "[sql] reusing SQL Server already listening on TCP $HOST_PORT"
  exit 0
fi

DOCKER_BIN="${DOCKER_BIN:-docker}"
if ! command -v "$DOCKER_BIN" >/dev/null 2>&1; then
  if [ -x "/mnt/e/Docker/resources/bin/docker.exe" ]; then
    DOCKER_BIN="/mnt/e/Docker/resources/bin/docker.exe"
  else
    echo "SQL Server is not listening and docker CLI was not found" >&2
    exit 1
  fi
fi

if [ -z "$SA_PASSWORD" ]; then
  echo "[sql] OG_SA_PASSWORD is required when the script must create a container" >&2
  exit 1
fi

if "$DOCKER_BIN" ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
  echo "[sql] container '$CONTAINER_NAME' already running"
else
  if "$DOCKER_BIN" ps -a --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
    echo "[sql] starting existing container '$CONTAINER_NAME'"
    "$DOCKER_BIN" start "$CONTAINER_NAME" >/dev/null
  else
    echo "[sql] creating container '$CONTAINER_NAME' on host port $HOST_PORT"
    "$DOCKER_BIN" run -d \
      --name "$CONTAINER_NAME" \
      --memory 1g \
      --memory-swap 1g \
      --cpus 2 \
      -e "ACCEPT_EULA=Y" \
      -e "MSSQL_PID=Developer" \
      -e "SA_PASSWORD=$SA_PASSWORD" \
      -e "MSSQL_MEMORY_LIMIT_MB=768" \
      -p "${HOST_PORT}:1433" \
      "$IMAGE"
  fi
fi

echo "[sql] waiting for engine readiness"
for attempt in $(seq 1 60); do
  if "$DOCKER_BIN" exec "$CONTAINER_NAME" \
       /opt/mssql-tools18/bin/sqlcmd -S 127.0.0.1 -U sa -P "$SA_PASSWORD" -C -Q "SELECT 1" \
       >/dev/null 2>&1; then
    echo "[sql] ready after ${attempt} probe(s)"
    "$DOCKER_BIN" exec "$CONTAINER_NAME" \
      /opt/mssql-tools18/bin/sqlcmd -S 127.0.0.1 -U sa -P "$SA_PASSWORD" -C \
      -Q "SELECT @@VERSION AS EngineVersion"
    exit 0
  fi
  sleep 2
done

echo "[sql] engine did not become ready in time" >&2
exit 1
