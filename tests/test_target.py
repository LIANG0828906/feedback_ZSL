import http.client
import json

from edge_sync.config import load_config
from edge_sync.target_service import start_target


def _ingest(host, port, record: dict, payload: bytes, bad_checksum: bool | None = None):
    connection = http.client.HTTPConnection(host, port, timeout=10)
    connection.connect()
    headers = {
        "X-Task-Id": record["task_id"],
        "X-Shard-Seq": str(record["shard_seq"]),
        "X-Start-Byte": str(record["start_byte"]),
        "X-End-Byte": str(record["end_byte"]),
        "X-Checksum": "deadbeef" if bad_checksum else record["checksum"],
    }
    connection.putrequest("POST", "/ingest")
    for key, value in headers.items():
        connection.putheader(key, value)
    connection.putheader("Transfer-Encoding", "chunked")
    connection.endheaders()
    connection.send(b"%X\r\n" % len(payload) + payload + b"\r\n0\r\n\r\n")
    response = connection.getresponse()
    body = json.loads(response.read().decode())
    connection.close()
    return response.status, body


def test_commit_once_and_range_lookup(tmp_path):
    db = str(tmp_path / "target.db")
    server, _, store = start_target("127.0.0.1", 0, db)
    host, port = server.server_address
    try:
        payload = b"edge-telemetry" * 64
        import hashlib

        record = {
            "task_id": "task-01",
            "shard_seq": 0,
            "start_byte": 0,
            "end_byte": len(payload),
            "checksum": hashlib.sha256(payload).hexdigest(),
        }
        first_status, first = _ingest(host, port, record, payload)
        second_status, second = _ingest(host, port, record, payload)
        assert first_status == 200 and first["duplicate"] is False
        assert second_status == 200 and second["duplicate"] is True
        assert store.committed_range("task-01", 0)["checksum"] == record["checksum"]
        status, body = _ingest(host, port, record, payload, bad_checksum=True)
        assert status == 409 and body["error"] == "SHARD_CHECKSUM_MISMATCH"
        assert store.committed_range("task-01", 0)["checksum"] == record["checksum"]
    finally:
        server.shutdown()
        server.server_close()
        store.close()


def test_health_endpoint(tmp_path):
    db = str(tmp_path / "target-health.db")
    server, _, store = start_target("127.0.0.1", 0, db)
    host, port = server.server_address
    try:
        connection = http.client.HTTPConnection(host, port, timeout=5)
        connection.request("GET", "/health")
        response = connection.getresponse()
        assert response.status == 200
        assert json.loads(response.read())["status"] == "ok"
        connection.close()
    finally:
        server.shutdown()
        server.server_close()
        store.close()
