#!/usr/bin/env bash
set -euo pipefail

KAFKA_HOME="${KAFKA_HOME:-}"
BOOTSTRAP="${KAFKA_BOOTSTRAP_SERVERS:-localhost:9092}"
TOPIC="${KAFKA_TOPIC:-order-events}"
PARTITIONS="${TOPIC_PARTITIONS:-6}"

if [[ -z "$KAFKA_HOME" || ! -x "$KAFKA_HOME/bin/kafka-topics.sh" ]]; then
  echo "set KAFKA_HOME to the Kafka 3.7 installation directory" >&2
  exit 1
fi

if "$KAFKA_HOME/bin/kafka-topics.sh" --bootstrap-server "$BOOTSTRAP" \
    --describe --topic "$TOPIC" >/dev/null 2>&1; then
  echo "topic $TOPIC already exists"
else
  "$KAFKA_HOME/bin/kafka-topics.sh" --bootstrap-server "$BOOTSTRAP" \
    --create --topic "$TOPIC" --partitions "$PARTITIONS" --replication-factor 1
fi
