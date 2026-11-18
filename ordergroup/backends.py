from __future__ import annotations

import gzip
import json
import os
import socket
import struct
import threading
import time
import zlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable


class BackendError(RuntimeError):
    pass


@dataclass(frozen=True)
class Record:
    offset: int
    key: str | None
    value: bytes
    timestamp_ms: int

    def as_dict(self, partition: int) -> dict[str, Any]:
        text = self.value.decode("utf-8", errors="replace")
        try:
            value: Any = json.loads(text)
        except json.JSONDecodeError:
            value = text
        return {
            "partition": partition,
            "offset": self.offset,
            "key": self.key,
            "value": value,
            "timestamp_ms": self.timestamp_ms,
        }


@dataclass
class FetchResult:
    records: list[Record] = field(default_factory=list)
    high_watermark: int = 0
    log_start_offset: int = 0


def _crc32c_table() -> list[int]:
    polynomial = 0x82F63B78
    table: list[int] = []
    for index in range(256):
        value = index
        for _ in range(8):
            value = (value >> 1) ^ polynomial if value & 1 else value >> 1
        table.append(value & 0xFFFFFFFF)
    return table


_CRC32C_TABLE = _crc32c_table()


def crc32c(data: bytes) -> int:
    crc = 0xFFFFFFFF
    for byte in data:
        crc = _CRC32C_TABLE[(crc ^ byte) & 0xFF] ^ (crc >> 8)
    return crc ^ 0xFFFFFFFF


def _encode_varint(value: int) -> bytes:
    value &= (1 << 64) - 1
    encoded = bytearray()
    while value > 0x7F:
        encoded.append((value & 0x7F) | 0x80)
        value >>= 7
    encoded.append(value)
    return bytes(encoded)


def _encode_zigzag(value: int) -> bytes:
    return _encode_varint((value << 1) ^ (value >> 63))


class _Reader:
    def __init__(self, data: bytes, position: int = 0) -> None:
        self.data = data
        self.position = position

    def _unpack(self, fmt: str) -> Any:
        size = struct.calcsize(fmt)
        if self.position + size > len(self.data):
            raise BackendError("truncated Kafka response")
        values = struct.unpack_from(fmt, self.data, self.position)
        self.position += size
        return values[0] if len(values) == 1 else values

    def int8(self) -> int:
        return int(self._unpack(">b"))

    def uint8(self) -> int:
        return int(self._unpack(">B"))

    def int16(self) -> int:
        return int(self._unpack(">h"))

    def int32(self) -> int:
        return int(self._unpack(">i"))

    def int64(self) -> int:
        return int(self._unpack(">q"))

    def string(self) -> str:
        size = self.int16()
        if size < 0:
            raise BackendError("unexpected null string")
        if self.position + size > len(self.data):
            raise BackendError("truncated Kafka string")
        value = self.data[self.position : self.position + size].decode("utf-8")
        self.position += size
        return value

    def nullable_string(self) -> str | None:
        size = self.int16()
        if size < 0:
            return None
        if self.position + size > len(self.data):
            raise BackendError("truncated Kafka string")
        value = self.data[self.position : self.position + size].decode("utf-8")
        self.position += size
        return value

    def bytes(self) -> bytes:
        size = self.int32()
        if size < 0:
            return b""
        if self.position + size > len(self.data):
            raise BackendError("truncated Kafka bytes field")
        value = self.data[self.position : self.position + size]
        self.position += size
        return value

    def array(self, callback: Any) -> list[Any]:
        size = self.int32()
        if size < 0:
            return []
        return [callback(self) for _ in range(size)]

    def remaining(self) -> int:
        return len(self.data) - self.position


class _Writer:
    def __init__(self) -> None:
        self.buffer = bytearray()

    def int8(self, value: int) -> "_Writer":
        self.buffer.extend(struct.pack(">b", value))
        return self

    def int16(self, value: int) -> "_Writer":
        self.buffer.extend(struct.pack(">h", value))
        return self

    def int32(self, value: int) -> "_Writer":
        self.buffer.extend(struct.pack(">i", value))
        return self

    def int64(self, value: int) -> "_Writer":
        self.buffer.extend(struct.pack(">q", value))
        return self

    def string(self, value: str) -> "_Writer":
        raw = value.encode("utf-8")
        self.int16(len(raw))
        self.buffer.extend(raw)
        return self

    def nullable_string(self, value: str | None) -> "_Writer":
        if value is None:
            self.int16(-1)
        else:
            self.string(value)
        return self

    def bytes(self, value: bytes) -> "_Writer":
        self.int32(len(value))
        self.buffer.extend(value)
        return self

    def array(self, values: Iterable[Any], callback: Any) -> "_Writer":
        items = list(values)
        self.int32(len(items))
        for item in items:
            callback(self, item)
        return self


def _decode_zigzag_varint(reader: _Reader) -> int:
    value = 0
    shift = 0
    while True:
        if reader.position >= len(reader.data):
            raise BackendError("truncated record varint")
        byte = reader.data[reader.position]
        reader.position += 1
        value |= (byte & 0x7F) << shift
        if not byte & 0x80:
            break
        shift += 7
        if shift > 63:
            raise BackendError("record varint is too long")
    return (value >> 1) ^ -(value & 1)


def encode_record_batch(
    records: list[tuple[str | None, bytes]],
    timestamp_ms: int | None = None,
) -> bytes:
    timestamp = int(time.time() * 1000) if timestamp_ms is None else timestamp_ms
    record_bytes = bytearray()
    for offset_delta, (key, value) in enumerate(records):
        body = bytearray()
        body.extend(struct.pack(">b", 0))
        body.extend(_encode_zigzag(0))
        body.extend(_encode_zigzag(offset_delta))
        if key is None:
            body.extend(_encode_zigzag(-1))
        else:
            key_raw = key.encode("utf-8")
            body.extend(_encode_zigzag(len(key_raw)))
            body.extend(key_raw)
        body.extend(_encode_zigzag(len(value)))
        body.extend(value)
        body.extend(_encode_zigzag(0))
        record_bytes.extend(_encode_zigzag(len(body)))
        record_bytes.extend(body)

    count = len(records)
    last_delta = max(0, count - 1)
    attributes = 0
    payload = bytearray()
    payload.extend(struct.pack(">h", attributes))
    payload.extend(struct.pack(">i", last_delta))
    payload.extend(struct.pack(">q", timestamp))
    payload.extend(struct.pack(">q", timestamp))
    payload.extend(struct.pack(">q", -1))
    payload.extend(struct.pack(">h", -1))
    payload.extend(struct.pack(">i", -1))
    payload.extend(struct.pack(">i", count))
    payload.extend(record_bytes)
    checksum = crc32c(bytes(payload))

    batch_body = bytearray()
    batch_body.extend(struct.pack(">i", 0))
    batch_body.extend(struct.pack(">b", 2))
    batch_body.extend(struct.pack(">I", checksum))
    batch_body.extend(payload)

    result = bytearray()
    result.extend(struct.pack(">q", 0))
    result.extend(struct.pack(">i", len(batch_body)))
    result.extend(batch_body)
    return bytes(result)


def decode_record_set(data: bytes) -> list[Record]:
    records: list[Record] = []
    position = 0
    while position + 12 <= len(data):
        base_offset = struct.unpack_from(">q", data, position)[0]
        batch_length = struct.unpack_from(">i", data, position + 8)[0]
        if batch_length < 0 or position + 12 + batch_length > len(data):
            break
        batch_end = position + 12 + batch_length
        magic_position = position + 16
        if magic_position >= batch_end:
            break
        magic = data[magic_position]
        if magic >= 2:
            if magic_position + 5 > batch_end:
                break
            attributes = struct.unpack_from(">h", data, magic_position + 5)[0]
            compression = attributes & 0x07
            first_timestamp = struct.unpack_from(
                ">q", data, magic_position + 11
            )[0]
            count = struct.unpack_from(">i", data, magic_position + 41)[0]
            record_position = magic_position + 45
            record_payload = data[record_position:batch_end]
            if compression == 1:
                record_payload = gzip.decompress(record_payload)
            elif compression != 0:
                raise BackendError(
                    f"unsupported Kafka compression codec {compression}"
                )
            reader = _Reader(record_payload)
            for _ in range(count):
                length = _decode_zigzag_varint(reader)
                record_end = reader.position + length
                reader.int8()
                timestamp_delta = _decode_zigzag_varint(reader)
                offset_delta = _decode_zigzag_varint(reader)
                key_size = _decode_zigzag_varint(reader)
                if key_size < 0:
                    key = None
                else:
                    if reader.position + key_size > record_end:
                        raise BackendError("truncated record key")
                    key = reader.data[
                        reader.position : reader.position + key_size
                    ].decode("utf-8", errors="replace")
                    reader.position += key_size
                value_size = _decode_zigzag_varint(reader)
                if value_size < 0:
                    value = b""
                else:
                    if reader.position + value_size > record_end:
                        raise BackendError("truncated record value")
                    value = reader.data[
                        reader.position : reader.position + value_size
                    ]
                    reader.position += value_size
                reader.position = record_end
                records.append(
                    Record(
                        offset=base_offset + offset_delta,
                        key=key,
                        value=value,
                        timestamp_ms=first_timestamp + timestamp_delta,
                    )
                )
        elif magic in (0, 1):
            reader = _Reader(data, position + 12)
            reader.int32()
            message_magic = reader.int8()
            attributes = reader.uint8()
            timestamp = reader.int64() if message_magic == 1 else 0
            key = reader.bytes()
            value = reader.bytes()
            compression = attributes & 0x07
            if compression == 1 and value:
                value = gzip.decompress(value)
            records.append(
                Record(
                    offset=base_offset,
                    key=key.decode("utf-8", errors="replace") if key else None,
                    value=value,
                    timestamp_ms=timestamp,
                )
            )
        else:
            break
        position = batch_end
    return records


def _parse_server(value: str) -> tuple[str, int]:
    if value.startswith("["):
        closing = value.find("]")
        if closing < 0:
            raise BackendError(f"invalid bootstrap server {value!r}")
        host = value[1:closing]
        port = int(value[closing + 2 :])
        return host, port
    host, separator, port_text = value.rpartition(":")
    if not separator:
        return value, 9092
    return host, int(port_text)


class KafkaWireBackend:
    """Small Kafka protocol client using stable non-flexible protocol versions."""

    def __init__(
        self,
        bootstrap_servers: str,
        topic: str,
        partitions: int,
        client_id: str = "order-group",
        request_timeout_ms: int = 5000,
        metadata_refresh_ms: int = 5000,
    ) -> None:
        self.bootstrap = [
            _parse_server(item.strip())
            for item in bootstrap_servers.split(",")
            if item.strip()
        ]
        if not self.bootstrap:
            raise BackendError("at least one Kafka bootstrap server is required")
        self.topic = topic
        self.partition_count = partitions
        self.client_id = client_id
        self.timeout = max(0.1, request_timeout_ms / 1000.0)
        self.metadata_refresh_seconds = max(0.1, metadata_refresh_ms / 1000.0)
        self._correlation_id = 0
        self._correlation_lock = threading.Lock()
        self._metadata_lock = threading.Lock()
        self._leaders: dict[int, tuple[str, int]] = {}
        self._metadata_at = 0.0

    def _next_correlation_id(self) -> int:
        with self._correlation_lock:
            self._correlation_id = (self._correlation_id + 1) & 0x7FFFFFFF
            return self._correlation_id

    def _request(
        self,
        server: tuple[str, int],
        api_key: int,
        api_version: int,
        body: bytes,
    ) -> _Reader:
        correlation = self._next_correlation_id()
        writer = _Writer()
        writer.int16(api_key)
        writer.int16(api_version)
        writer.int32(correlation)
        writer.nullable_string(self.client_id)
        request = bytes(writer.buffer) + body
        packet = struct.pack(">i", len(request)) + request
        try:
            with socket.create_connection(server, timeout=self.timeout) as sock:
                sock.settimeout(self.timeout)
                sock.sendall(packet)
                size_data = self._recv_exact(sock, 4)
                size = struct.unpack(">i", size_data)[0]
                if size < 4 or size > 128 * 1024 * 1024:
                    raise BackendError(f"invalid Kafka response size {size}")
                response = self._recv_exact(sock, size)
        except (OSError, socket.timeout) as exc:
            raise BackendError(f"Kafka connection to {server[0]}:{server[1]} failed: {exc}") from exc
        reader = _Reader(response)
        received_correlation = reader.int32()
        if received_correlation != correlation:
            raise BackendError(
                f"Kafka correlation mismatch: expected {correlation}, got {received_correlation}"
            )
        return reader

    @staticmethod
    def _recv_exact(sock: socket.socket, size: int) -> bytes:
        chunks = bytearray()
        while len(chunks) < size:
            chunk = sock.recv(size - len(chunks))
            if not chunk:
                raise BackendError("Kafka connection closed before response completed")
            chunks.extend(chunk)
        return bytes(chunks)

    def _metadata_request(self) -> None:
        writer = _Writer()
        writer.int32(1)
        writer.string(self.topic)
        last_error: Exception | None = None
        for server in self.bootstrap:
            try:
                reader = self._request(server, 3, 0, bytes(writer.buffer))
                brokers: dict[int, tuple[str, int]] = {}
                broker_count = reader.int32()
                for _ in range(broker_count):
                    node_id = reader.int32()
                    host = reader.string()
                    port = reader.int32()
                    brokers[node_id] = (host, port)
                topic_count = reader.int32()
                leaders: dict[int, tuple[str, int]] = {}
                for _ in range(topic_count):
                    error_code = reader.int16()
                    name = reader.string()
                    partition_count = reader.int32()
                    for _ in range(partition_count):
                        partition_error = reader.int16()
                        partition_id = reader.int32()
                        leader_id = reader.int32()
                        replica_count = reader.int32()
                        for _ in range(replica_count):
                            reader.int32()
                        isr_count = reader.int32()
                        for _ in range(isr_count):
                            reader.int32()
                        if (
                            name == self.topic
                            and error_code == 0
                            and partition_error == 0
                            and leader_id in brokers
                        ):
                            leaders[partition_id] = brokers[leader_id]
                if error_code != 0:
                    raise BackendError(
                        f"Kafka metadata error {error_code} for topic {self.topic!r}"
                    )
                if not leaders:
                    raise BackendError(
                        f"Kafka topic {self.topic!r} has no available leaders"
                    )
                self._leaders = leaders
                self._metadata_at = time.monotonic()
                return
            except Exception as exc:  # Try the next bootstrap endpoint.
                last_error = exc
        raise BackendError(f"Kafka metadata request failed: {last_error}")

    def _leader(self, partition: int, force_refresh: bool = False) -> tuple[str, int]:
        with self._metadata_lock:
            stale = time.monotonic() - self._metadata_at > self.metadata_refresh_seconds
            if force_refresh or stale or partition not in self._leaders:
                self._metadata_request()
            leader = self._leaders.get(partition)
            if leader is None:
                raise BackendError(
                    f"Kafka topic {self.topic!r} does not have partition {partition}"
                )
            return leader

    def fetch(
        self,
        partition: int,
        offset: int,
        max_bytes: int = 1048576,
        max_records: int = 250,
    ) -> FetchResult:
        for attempt in range(3):
            leader = self._leader(partition, force_refresh=attempt > 0)
            writer = _Writer()
            writer.int32(-1)
            writer.int32(100)
            writer.int32(1)
            writer.int32(max_bytes)
            writer.int8(0)
            writer.int32(1)
            writer.string(self.topic)
            writer.int32(1)
            writer.int32(partition)
            writer.int64(max(0, offset))
            writer.int32(max_bytes)
            try:
                reader = self._request(leader, 1, 4, bytes(writer.buffer))
                reader.int32()
                topic_count = reader.int32()
                for _ in range(topic_count):
                    reader.string()
                    partition_count = reader.int32()
                    for _ in range(partition_count):
                        response_partition = reader.int32()
                        error_code = reader.int16()
                        high_watermark = reader.int64()
                        reader.int64()
                        aborted_count = reader.int32()
                        if aborted_count > 0:
                            for _ in range(aborted_count):
                                reader.int64()
                                reader.int64()
                        raw_records = reader.bytes()
                        if response_partition != partition:
                            continue
                        if error_code in (6, 9):
                            raise BackendError(
                                f"Kafka fetch retryable error {error_code}"
                            )
                        if error_code != 0:
                            raise BackendError(
                                f"Kafka fetch error {error_code} on partition {partition}"
                            )
                        parsed = [
                            record
                            for record in decode_record_set(raw_records)
                            if record.offset >= offset
                        ]
                        return FetchResult(
                            records=parsed[:max_records],
                            high_watermark=high_watermark,
                            log_start_offset=0,
                        )
                raise BackendError("Kafka fetch response omitted the requested partition")
            except BackendError:
                if attempt == 2:
                    raise
                time.sleep(0.05 * (attempt + 1))
        raise BackendError("Kafka fetch failed")

    def list_offsets(
        self, partitions: Iterable[int], timestamp: int
    ) -> dict[int, int]:
        requested = sorted(set(partitions))
        if not requested:
            return {}
        leaders: dict[tuple[str, int], list[int]] = {}
        for partition in requested:
            leaders.setdefault(self._leader(partition), []).append(partition)
        result: dict[int, int] = {}
        for leader, leader_partitions in leaders.items():
            writer = _Writer()
            writer.int32(-1)
            writer.int32(1)
            writer.string(self.topic)
            writer.int32(len(leader_partitions))
            for partition in leader_partitions:
                writer.int32(partition)
                writer.int64(timestamp)
            reader = self._request(leader, 2, 1, bytes(writer.buffer))
            topic_count = reader.int32()
            for _ in range(topic_count):
                reader.string()
                partition_count = reader.int32()
                for _ in range(partition_count):
                    partition = reader.int32()
                    error_code = reader.int16()
                    reader.int64()
                    offset = reader.int64()
                    if error_code == 0:
                        result[partition] = offset
        missing = set(requested) - set(result)
        if missing:
            raise BackendError(
                f"Kafka list-offsets response omitted partitions {sorted(missing)}"
            )
        return result

    def end_offsets(self, partitions: Iterable[int]) -> dict[int, int]:
        return self.list_offsets(partitions, -1)

    def earliest_offsets(self, partitions: Iterable[int]) -> dict[int, int]:
        return self.list_offsets(partitions, -2)

    def produce(
        self,
        partition: int,
        records: list[tuple[str | None, bytes]],
        timestamp_ms: int | None = None,
    ) -> int:
        if not records:
            return 0
        batch = encode_record_batch(records, timestamp_ms=timestamp_ms)
        for attempt in range(4):
            leader = self._leader(partition, force_refresh=attempt > 0)
            writer = _Writer()
            writer.nullable_string(None)
            writer.int16(1)
            writer.int32(10000)
            writer.int32(1)
            writer.string(self.topic)
            writer.int32(1)
            writer.int32(partition)
            writer.bytes(batch)
            try:
                reader = self._request(leader, 0, 3, bytes(writer.buffer))
                topic_count = reader.int32()
                for _ in range(topic_count):
                    reader.string()
                    partition_count = reader.int32()
                    for _ in range(partition_count):
                        response_partition = reader.int32()
                        error_code = reader.int16()
                        base_offset = reader.int64()
                        reader.int64()
                        if response_partition != partition:
                            continue
                        if error_code in (5, 6, 9):
                            raise BackendError(
                                f"Kafka produce retryable error {error_code}"
                            )
                        if error_code != 0:
                            raise BackendError(
                                f"Kafka produce error {error_code} on partition {partition}"
                            )
                        return base_offset
                raise BackendError("Kafka produce response omitted the requested partition")
            except BackendError:
                if attempt == 3:
                    raise
                time.sleep(0.1 * (attempt + 1))
        raise BackendError("Kafka produce failed")

    def close(self) -> None:
        return None


class SimulatedKafkaBackend:
    """Partitioned JSONL backend that mirrors the Kafka offsets used by the task."""

    def __init__(self, directory: str | os.PathLike[str], partitions: int) -> None:
        self.directory = Path(directory)
        self.partition_count = partitions
        self.directory.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._indexes: dict[int, list[int]] = {}
        self._end_offsets: dict[int, int] = {}
        self._load()

    def _data_path(self, partition: int) -> Path:
        return self.directory / f"partition-{partition:02d}.jsonl"

    def _index_path(self, partition: int) -> Path:
        return self.directory / f"partition-{partition:02d}.idx"

    def _load(self) -> None:
        for partition in range(self.partition_count):
            data_path = self._data_path(partition)
            index_path = self._index_path(partition)
            indexes: list[int] = []
            if index_path.exists():
                raw = index_path.read_bytes()
                indexes = [
                    struct.unpack_from(">q", raw, position)[0]
                    for position in range(0, len(raw) - 7, 8)
                ]
            elif data_path.exists():
                with data_path.open("rb") as handle:
                    while True:
                        position = handle.tell()
                        line = handle.readline()
                        if not line:
                            break
                        indexes.append(position)
            self._indexes[partition] = indexes
            self._end_offsets[partition] = len(indexes)

    def fetch(
        self,
        partition: int,
        offset: int,
        max_bytes: int = 1048576,
        max_records: int = 250,
    ) -> FetchResult:
        if partition < 0 or partition >= self.partition_count:
            raise BackendError(f"partition {partition} is out of range")
        with self._lock:
            indexes = self._indexes[partition]
            end = len(indexes)
            if offset < 0:
                offset = 0
            records: list[Record] = []
            size = 0
            data_path = self._data_path(partition)
            if offset < end and data_path.exists():
                with data_path.open("rb") as handle:
                    for current in range(offset, min(end, offset + max_records)):
                        handle.seek(indexes[current])
                        line = handle.readline()
                        if not line:
                            break
                        if records and size + len(line) > max_bytes:
                            break
                        text = line.decode("utf-8").rstrip("\r\n")
                        key: str | None = None
                        value: bytes = line.rstrip(b"\r\n")
                        try:
                            parsed = json.loads(text)
                            key = str(parsed.get("order_id", current))
                            value = json.dumps(
                                parsed,
                                ensure_ascii=False,
                                separators=(",", ":"),
                                sort_keys=True,
                            ).encode("utf-8")
                        except (json.JSONDecodeError, AttributeError):
                            key = None
                        records.append(
                            Record(
                                offset=current,
                                key=key,
                                value=value,
                                timestamp_ms=0,
                            )
                        )
                        size += len(line)
            return FetchResult(
                records=records,
                high_watermark=end,
                log_start_offset=0,
            )

    def end_offsets(self, partitions: Iterable[int]) -> dict[int, int]:
        with self._lock:
            return {partition: self._end_offsets[partition] for partition in partitions}

    def earliest_offsets(self, partitions: Iterable[int]) -> dict[int, int]:
        return {partition: 0 for partition in partitions}

    def produce(
        self,
        partition: int,
        records: list[tuple[str | None, bytes]],
        timestamp_ms: int | None = None,
    ) -> int:
        with self._lock:
            data_path = self._data_path(partition)
            index_path = self._index_path(partition)
            indexes = self._indexes[partition]
            base_offset = len(indexes)
            with data_path.open("ab") as data_handle, index_path.open("ab") as index_handle:
                for key, value in records:
                    position = data_handle.tell()
                    data_handle.write(value)
                    data_handle.write(b"\n")
                    index_handle.write(struct.pack(">q", position))
                    indexes.append(position)
            self._end_offsets[partition] = len(indexes)
            return base_offset

    def close(self) -> None:
        return None


def create_backend(config: Any) -> KafkaWireBackend | SimulatedKafkaBackend:
    kind = config.backend_kind
    if kind == "simulator":
        return SimulatedKafkaBackend(config.simulator_dir, config.partitions)
    if kind != "kafka":
        raise BackendError(f"unsupported backend kind {kind!r}")
    return KafkaWireBackend(
        bootstrap_servers=str(config.get("kafka", "bootstrap_servers")),
        topic=config.topic,
        partitions=config.partitions,
        client_id=str(config.get("kafka", "client_id", "order-group")),
        request_timeout_ms=int(config.get("kafka", "request_timeout_ms", 5000)),
        metadata_refresh_ms=int(config.get("kafka", "metadata_refresh_ms", 5000)),
    )

