from __future__ import annotations

import hashlib
import json
import shutil
import struct
import tempfile
from functools import lru_cache
from pathlib import Path
from typing import Any, Callable

from mediaforge.errors import WorkbenchError
from mediaforge.jsonutil import canonical_json_bytes, sha256_bytes
from mediaforge.pipeline import LoadedSample, load_sample


ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"
GROUP_IDS = ("g01_neon_city", "g02_clockwork", "g03_tidal")


@lru_cache(maxsize=16)
def loaded(sample_id: str = "g01_neon_city", version: str = "v3") -> LoadedSample:
    return load_sample(SAMPLES / sample_id, version)


def copy_group(sample_id: str = "g01_neon_city") -> Path:
    target = Path(tempfile.mkdtemp(prefix="mf-sample-")) / sample_id
    shutil.copytree(SAMPLES / sample_id, target)
    return target


def read_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def write_json(path: Path, value: Any) -> None:
    path.write_bytes(canonical_json_bytes(value) + b"\n")


def expect_error(code: str, function: Callable[[], Any]) -> WorkbenchError:
    try:
        function()
    except WorkbenchError as exc:
        if exc.code != code:
            raise AssertionError(f"expected {code}, got {exc.code} at {exc.field_path}: {exc.message}")
        return exc
    raise AssertionError(f"expected {code}, no error was raised")


def tree_hashes(root: Path) -> dict[str, str]:
    return {
        path.relative_to(root).as_posix(): sha256_bytes(path.read_bytes())
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


def patch_package_header(
    artifact: bytes,
    mutator: Callable[[dict[str, Any]], None],
) -> bytes:
    magic = artifact[:4]
    header_length = struct.unpack("<Q", artifact[4:12])[0]
    header = json.loads(artifact[12 : 12 + header_length].decode("utf-8"))
    mutator(header)
    header_bytes = canonical_json_bytes(header)
    payload = artifact[12 + header_length : -32]
    body = magic + struct.pack("<Q", len(header_bytes)) + header_bytes + payload
    return body + bytes.fromhex(sha256_bytes(body))


def tree_digest(root: Path) -> str:
    digest = hashlib.sha256()
    for relative, file_hash in tree_hashes(root).items():
        digest.update(relative.encode("utf-8"))
        digest.update(b"\0")
        digest.update(file_hash.encode("ascii"))
        digest.update(b"\n")
    return digest.hexdigest()
