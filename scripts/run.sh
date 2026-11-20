#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

LOCAL_JDK="$ROOT/.tools/jdk17/usr/lib/jvm/java-17-openjdk-amd64"
if [[ -z "${JAVA_HOME:-}" && -x "$LOCAL_JDK/bin/java" ]]; then
  export JAVA_HOME="$LOCAL_JDK"
fi
JAVA="java"
if [[ -n "${JAVA_HOME:-}" && -x "$JAVA_HOME/bin/java" ]]; then
  JAVA="$JAVA_HOME/bin/java"
fi
if ! command -v "$JAVA" >/dev/null 2>&1; then
  echo "Java 17+ was not found." >&2
  exit 1
fi

JAR="$ROOT/target/edgekv-index.jar"
if [[ ! -f "$JAR" ]]; then
  "$ROOT/scripts/build.sh" -q package
fi

exec "$JAVA" -jar "$JAR" "$@"
