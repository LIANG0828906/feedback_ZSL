"""规范化序列化与确定性哈希。

真值仅来自字段序列化内容：固定字段顺序（sort_keys）、最紧凑分隔符、
UTF-8 编码；不引入时间戳、文件大小或任何环境相关量。
"""

import hashlib
import json

GENESIS_CHAIN = "0" * 64


def canonical_bytes(obj) -> bytes:
    return json.dumps(obj, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False).encode("utf-8")


def hash_obj(obj) -> str:
    return hashlib.sha256(canonical_bytes(obj)).hexdigest()


def chain_hash(prev_chain: str, state_hash: str) -> str:
    return hashlib.sha256(
        f"{prev_chain}:{state_hash}".encode("ascii")).hexdigest()
