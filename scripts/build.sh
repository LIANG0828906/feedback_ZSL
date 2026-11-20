#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

LOCAL_JDK="$ROOT/.tools/jdk17/usr/lib/jvm/java-17-openjdk-amd64"
if [[ -z "${JAVA_HOME:-}" && -x "$LOCAL_JDK/bin/java" ]]; then
  export JAVA_HOME="$LOCAL_JDK"
elif [[ ! -x "${JAVA_HOME:-}/bin/java" ]] && command -v java >/dev/null 2>&1; then
  export JAVA_HOME="$(dirname "$(dirname "$(readlink -f "$(command -v java)")")")"
fi

if [[ -z "${JAVA_HOME:-}" || ! -x "$JAVA_HOME/bin/javac" ]]; then
  echo "JAVA_HOME must point to a Java 17+ JDK (javac is required)." >&2
  exit 1
fi
export PATH="$JAVA_HOME/bin:$PATH"

MAVEN="$ROOT/.tools/maven/bin/mvn"
if [[ ! -x "$MAVEN" ]]; then
  MAVEN="$(command -v mvn || true)"
fi
if [[ -z "$MAVEN" ]]; then
  echo "Maven was not found. Install Maven 3.9+ or unpack it into .tools/maven." >&2
  exit 1
fi

MAVEN_OPTS="${MAVEN_OPTS:-} -Djavax.net.ssl.trustStore=$JAVA_HOME/lib/security/cacerts -Djavax.net.ssl.trustStorePassword=changeit"
export MAVEN_OPTS

OFFLINE=()
if [[ -d "$ROOT/.m2repo/org/junit" ]]; then
  OFFLINE=(-o)
fi

if [[ "$#" -eq 0 ]]; then
  set -- package
fi
exec "$MAVEN" "${OFFLINE[@]}" "$@"
