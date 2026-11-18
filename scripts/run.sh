#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
JAR="$ROOT/target/subscription-delivery-1.0.0.jar"

if [[ ! -f "$JAR" ]]; then
  echo "missing $JAR; run: mvn -DskipTests package" >&2
  exit 1
fi

if [[ -n "${JAVA_HOME:-}" && -x "$JAVA_HOME/bin/java" ]]; then
  JAVA="$JAVA_HOME/bin/java"
else
  JAVA="$(command -v java || true)"
fi

if [[ -z "$JAVA" ]]; then
  echo "JAVA_HOME or java is required (JDK 21)" >&2
  exit 1
fi

exec "$JAVA" \
  -Xms128m \
  -Xmx512m \
  -XX:MaxMetaspaceSize=192m \
  -XX:ActiveProcessorCount=2 \
  -XX:+UseG1GC \
  -XX:MaxGCPauseMillis=100 \
  -jar "$JAR" "$@"
