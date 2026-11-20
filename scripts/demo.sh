#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

"$ROOT/scripts/build.sh" -q package

LOCAL_JDK="$ROOT/.tools/jdk17/usr/lib/jvm/java-17-openjdk-amd64"
if [[ -z "${JAVA_HOME:-}" && -x "$LOCAL_JDK/bin/java" ]]; then
  export JAVA_HOME="$LOCAL_JDK"
fi
JAVA="java"
if [[ -n "${JAVA_HOME:-}" && -x "$JAVA_HOME/bin/java" ]]; then
  JAVA="$JAVA_HOME/bin/java"
fi

exec "$JAVA" -cp "$ROOT/target/edgekv-index.jar" io.edgekv.demo.DemoRunner --root "$ROOT"
