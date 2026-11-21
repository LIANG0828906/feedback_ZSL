"""切片原子提交、索引清单哈希链与强制终止恢复。

切片一律先写 ``.tmp`` 临时文件再 :func:`os.replace` 原子重命名；清单按
提交顺序追加，每行携带前一行哈希形成哈希链。载入时校验字段完整性、
哈希链与切片文件内容，任何断链/字段缺失/文件损坏都以
``E_RECOVERY_INVALID`` 中止。
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

from .errors import AuditError, ErrorCode
from .slicing import SliceArtifact, serialize_artifact
from .util import canonical_json, content_hash

MANIFEST_NAME = "manifest.jsonl"
GENESIS_HASH = "0" * 64
REQUIRED_FIELDS = (
    "tier",
    "slice_index",
    "param_version",
    "graph_version",
    "content_hash",
    "prev_hash",
    "row_hash",
    "file_name",
)


def slice_file_name(tier: str, param_version: int, slice_index: int) -> str:
    return f"{tier}__v{param_version}__s{slice_index:06d}.slice.json"


def _recovery_fail(message: str, location: tuple[str, ...], details: dict) -> AuditError:
    return AuditError(ErrorCode.E_RECOVERY_INVALID, message, location, details)


@dataclass(frozen=True)
class ManifestRow:
    data: dict

    @property
    def key(self) -> tuple[str, int, int]:
        return (self.data["tier"], self.data["param_version"], self.data["slice_index"])


class SliceStore:
    """切片文件与清单的本地存储。"""

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.slices_dir = self.root / "slices"
        self.manifest_path = self.root / MANIFEST_NAME
        self.slices_dir.mkdir(parents=True, exist_ok=True)
        self.root.mkdir(parents=True, exist_ok=True)

    # ---- 切片文件 -------------------------------------------------------
    def slice_path(self, tier: str, param_version: int, slice_index: int) -> Path:
        return self.slices_dir / slice_file_name(tier, param_version, slice_index)

    def write_slice_atomic(self, artifact: SliceArtifact) -> None:
        """临时文件 + 原子重命名；重复提交同一内容不产生额外写入。"""
        target = self.slice_path(
            artifact.tier, artifact.param_version, artifact.slice_index
        )
        if target.exists():
            existing = target.read_bytes()
            if bytes_hash(existing) == bytes_hash(serialize_artifact(artifact)):
                return
        payload = serialize_artifact(artifact)
        tmp = target.with_suffix(".slice.json.tmp")
        tmp.write_bytes(payload)
        os.replace(tmp, target)  # 同目录原子重命名

    def read_slice_bytes(
        self, tier: str, param_version: int, slice_index: int
    ) -> bytes | None:
        path = self.slice_path(tier, param_version, slice_index)
        if not path.exists():
            return None
        return path.read_bytes()

    def cleanup_temp_files(self) -> list[str]:
        """崩溃后清理未完成的临时文件，释放文件句柄残留。"""
        removed = []
        for tmp in self.slices_dir.glob("*.tmp"):
            tmp.unlink()
            removed.append(str(tmp))
        return removed

    # ---- 清单哈希链 -----------------------------------------------------
    @staticmethod
    def row_payload(row: dict) -> dict:
        """参与行哈希的字段（不含 prev_hash/row_hash 自身）。"""
        return {k: v for k, v in row.items() if k not in ("prev_hash", "row_hash")}

    @classmethod
    def compute_row_hash(cls, row: dict) -> str:
        return content_hash(cls.row_payload(row))

    def append_manifest_row(self, row: dict, prev_hash: str) -> dict:
        record = dict(row)
        record["prev_hash"] = prev_hash
        record["row_hash"] = self.compute_row_hash(record)
        line = canonical_json(record) + b"\n"
        tmp = self.manifest_path.with_suffix(".jsonl.tmp")
        if self.manifest_path.exists():
            # 先复制再追加，保证清单自身也是原子替换。
            tmp.write_bytes(self.manifest_path.read_bytes() + line)
        else:
            tmp.write_bytes(line)
        os.replace(tmp, self.manifest_path)
        return record

    def load_manifest(self) -> list[ManifestRow]:
        """校验并载入清单，断链/字段缺失/文件损坏立即中止。"""
        if not self.manifest_path.exists():
            return []
        rows: list[ManifestRow] = []
        prev_hash = GENESIS_HASH
        with self.manifest_path.open("r", encoding="utf-8") as handle:
            for line_no, raw_line in enumerate(handle, start=1):
                line = raw_line.strip()
                if not line:
                    continue
                loc = ("manifest", f"line {line_no}")
                try:
                    row = json.loads(line)
                except json.JSONDecodeError as exc:
                    raise _recovery_fail(
                        "清单行不是合法 JSON", loc, {"error": exc.msg}
                    ) from exc
                missing = [f for f in REQUIRED_FIELDS if f not in row]
                if missing:
                    raise _recovery_fail(
                        "清单字段缺失", loc, {"missing": missing}
                    )
                if row["prev_hash"] != prev_hash:
                    raise _recovery_fail(
                        "清单哈希链断裂",
                        loc,
                        {"expected_prev": prev_hash, "actual_prev": row["prev_hash"]},
                    )
                actual_row_hash = self.compute_row_hash(row)
                if actual_row_hash != row["row_hash"]:
                    raise _recovery_fail(
                        "清单行哈希校验失败",
                        loc,
                        {"expected": row["row_hash"], "actual": actual_row_hash},
                    )
                rows.append(ManifestRow(row))
                prev_hash = row["row_hash"]
        return rows

    def verify_slice_file(self, row: ManifestRow) -> None:
        """校验清单对应的切片文件存在且内容哈希一致。"""
        data = row.data
        blob = self.read_slice_bytes(
            data["tier"], data["param_version"], data["slice_index"]
        )
        loc = (
            "slices",
            slice_file_name(data["tier"], data["param_version"], data["slice_index"]),
        )
        if blob is None:
            raise _recovery_fail("切片文件缺失", loc, {"manifest_key": row.key})
        # 切片文件是规范 JSON，重新序列化计算内容哈希与清单做双重校验。
        try:
            payload = json.loads(blob)
        except json.JSONDecodeError as exc:
            raise _recovery_fail("切片文件损坏", loc, {"error": exc.msg}) from exc
        file_content_hash = content_hash(payload)
        if file_content_hash != data["content_hash"]:
            raise _recovery_fail(
                "切片内容哈希与清单不一致",
                loc,
                {
                    "manifest_hash": data["content_hash"],
                    "file_hash": file_content_hash,
                },
            )

    def recover(self) -> tuple[list[ManifestRow], list[str]]:
        """启动恢复：清理临时文件并校验完整清单与全部切片。"""
        removed_tmp = self.cleanup_temp_files()
        rows = self.load_manifest()
        for row in rows:
            self.verify_slice_file(row)
        return rows, removed_tmp
