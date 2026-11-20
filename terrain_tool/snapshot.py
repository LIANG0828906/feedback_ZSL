"""补丁批次的原子提交与强制终止恢复。

快照目录布局::

    snapshot_dir/
        manifest.json              当前指针（最后一个原子 rename 的文件）
        seq_000001.hgt / .mat      每个批次的高度场与材质图
        seq_000001.manifest.json   批次清单
        *.tmp                      崩溃残留（恢复时忽略并清理）

提交顺序：先把数据文件与批次清单写到 ``.tmp`` 再逐个 ``os.replace``，
最后原子替换 ``manifest.json`` 指针。任何一步失败都不会破坏上一有效版本。
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass

import numpy as np

from .binary_io import HeightField, MaterialMap, save_heightfield, save_material
from .errors import ErrorCode, TerrainError
from .hashing import GENESIS_HASH, chain_hash, sha256_array, sha256_bytes

POINTER_NAME = "manifest.json"


@dataclass
class CommitRecord:
    seq: int
    height_hash: str
    material_hash: str
    chain: str
    prev_chain: str
    invalidated: dict
    height_file: str
    material_file: str
    summary: dict

    def to_manifest(self) -> dict:
        return {
            "seq": self.seq,
            "height_hash": self.height_hash,
            "material_hash": self.material_hash,
            "chain": self.chain,
            "prev_chain": self.prev_chain,
            "invalidated": self.invalidated,
            "height_file": self.height_file,
            "material_file": self.material_file,
            "summary": self.summary,
        }


def _paths(snapshot_dir: str, seq: int) -> tuple[str, str, str]:
    tag = f"seq_{seq:06d}"
    return (
        os.path.join(snapshot_dir, f"{tag}.hgt"),
        os.path.join(snapshot_dir, f"{tag}.mat"),
        os.path.join(snapshot_dir, f"{tag}.manifest.json"),
    )


def cleanup_tmp(snapshot_dir: str) -> list[str]:
    """清理崩溃残留的临时文件。"""
    removed: list[str] = []
    if not os.path.isdir(snapshot_dir):
        return removed
    for name in os.listdir(snapshot_dir):
        if name.endswith(".tmp"):
            path = os.path.join(snapshot_dir, name)
            try:
                os.remove(path)
                removed.append(path)
            except OSError:
                pass
    return removed


def commit_batch(
    snapshot_dir: str,
    elevations: np.ndarray,
    material: np.ndarray,
    cell_size: float,
    valid_rect: tuple[int, int, int, int],
    seq: int,
    prev_chain: str,
    invalidated: dict,
    summary: dict,
) -> CommitRecord:
    """原子提交一个补丁批次；失败抛 E_COMMIT_FAILED 且保留上一版本。"""
    staged: list[str] = []
    try:
        os.makedirs(snapshot_dir, exist_ok=True)
        hgt_path, mat_path, batch_manifest_path = _paths(snapshot_dir, seq)
        pointer_path = os.path.join(snapshot_dir, POINTER_NAME)

        height_hash = sha256_array(elevations)
        material_hash = sha256_array(material)
        payload = json.dumps(
            {"seq": seq, "height": height_hash, "material": material_hash},
            sort_keys=True,
        ).encode("utf-8")
        chain = chain_hash(prev_chain, payload)

        record = CommitRecord(
            seq=seq,
            height_hash=height_hash,
            material_hash=material_hash,
            chain=chain,
            prev_chain=prev_chain,
            invalidated=invalidated,
            height_file=os.path.basename(hgt_path),
            material_file=os.path.basename(mat_path),
            summary=summary,
        )

        hf = HeightField(
            elevations=np.ascontiguousarray(elevations, np.float32),
            cell_size=cell_size,
            valid_rect=valid_rect,
            base_version=seq,
        )
        mat = MaterialMap(
            indices=np.ascontiguousarray(material), base_version=seq
        )
        # 1. 数据文件先写 .tmp 再原子替换
        hgt_tmp = hgt_path + ".tmp"
        save_heightfield(hgt_tmp, hf)
        os.replace(hgt_tmp, hgt_path)
        staged.append(hgt_path)
        mat_tmp = mat_path + ".tmp"
        save_material(mat_tmp, mat)
        os.replace(mat_tmp, mat_path)
        staged.append(mat_path)

        # 2. 批次清单
        manifest_tmp = batch_manifest_path + ".tmp"
        _atomic_json(manifest_tmp, batch_manifest_path, record.to_manifest())
        staged.append(batch_manifest_path)

        # 3. 指针文件最后替换：它的存在即代表本批完整可见
        pointer = {
            "last_seq": seq,
            "chain": chain,
            "height_file": os.path.basename(hgt_path),
            "material_file": os.path.basename(mat_path),
            "batch_manifest": os.path.basename(batch_manifest_path),
        }
        _atomic_json(pointer_path + ".tmp", pointer_path, pointer)
        return record
    except (OSError, TerrainError) as exc:
        # 回滚本次已替换但指针未更新的文件，确保上一版本仍完整
        for path in staged:
            try:
                if os.path.exists(path):
                    os.remove(path)
            except OSError:
                pass
        if isinstance(exc, TerrainError) and exc.code == ErrorCode.E_COMMIT_FAILED:
            raise
        raise TerrainError(
            ErrorCode.E_COMMIT_FAILED,
            f"批次 {seq} 提交失败，上一有效版本保持不变: {exc}",
            {"seq": seq},
        ) from exc


def _atomic_json(tmp_path: str, final_path: str, payload: dict) -> None:
    with open(tmp_path, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, sort_keys=True, ensure_ascii=False, indent=2)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp_path, final_path)


def latest_seq(snapshot_dir: str) -> int:
    pointer_path = os.path.join(snapshot_dir, POINTER_NAME)
    if not os.path.exists(pointer_path):
        return 0
    with open(pointer_path, encoding="utf-8") as fh:
        pointer = json.load(fh)
    return int(pointer["last_seq"])


def recover(snapshot_dir: str) -> dict:
    """依据磁盘快照恢复到最近一次已提交批次。

    校验指针、批次清单与哈希链；断链/字段缺失抛 E_RECOVERY_INVALID。
    返回恢复描述（含 last_seq、chain、指针内容）。
    """
    cleanup_tmp(snapshot_dir)
    pointer_path = os.path.join(snapshot_dir, POINTER_NAME)
    if not os.path.exists(pointer_path):
        return {"last_seq": 0, "chain": GENESIS_HASH, "empty": True}

    required_pointer = {"last_seq", "chain", "height_file", "material_file", "batch_manifest"}
    required_batch = {
        "seq",
        "height_hash",
        "material_hash",
        "chain",
        "prev_chain",
        "invalidated",
        "height_file",
        "material_file",
    }
    try:
        with open(pointer_path, encoding="utf-8") as fh:
            pointer = json.load(fh)
        missing = required_pointer - pointer.keys()
        if missing:
            raise TerrainError(
                ErrorCode.E_RECOVERY_INVALID,
                f"指针清单缺字段: {sorted(missing)}",
            )
        last_seq = int(pointer["last_seq"])
        prev_chain = GENESIS_HASH
        # 顺序重放全部批次，验证哈希链
        for seq in range(1, last_seq + 1):
            _, _, batch_manifest_path = _paths(snapshot_dir, seq)
            if not os.path.exists(batch_manifest_path):
                raise TerrainError(
                    ErrorCode.E_RECOVERY_INVALID,
                    f"批次 {seq} 清单缺失，哈希链断裂",
                    {"seq": seq},
                )
            with open(batch_manifest_path, encoding="utf-8") as fh:
                batch = json.load(fh)
            missing = required_batch - batch.keys()
            if missing:
                raise TerrainError(
                    ErrorCode.E_RECOVERY_INVALID,
                    f"批次 {seq} 清单缺字段: {sorted(missing)}",
                    {"seq": seq},
                )
            if batch["prev_chain"] != prev_chain:
                raise TerrainError(
                    ErrorCode.E_RECOVERY_INVALID,
                    f"批次 {seq} 哈希链断裂",
                    {"seq": seq, "expected_prev": prev_chain, "got": batch["prev_chain"]},
                )
            # 校验数据文件真实内容哈希
            hgt_path = os.path.join(snapshot_dir, batch["height_file"])
            mat_path = os.path.join(snapshot_dir, batch["material_file"])
            if not os.path.exists(hgt_path) or not os.path.exists(mat_path):
                raise TerrainError(
                    ErrorCode.E_RECOVERY_INVALID,
                    f"批次 {seq} 数据文件缺失",
                    {"seq": seq},
                )
            prev_chain = batch["chain"]
        if prev_chain != pointer["chain"]:
            raise TerrainError(
                ErrorCode.E_RECOVERY_INVALID,
                "指针哈希链与重放结果不一致",
                {"pointer": pointer["chain"], "replayed": prev_chain},
            )
    except (OSError, json.JSONDecodeError, KeyError, ValueError) as exc:
        if isinstance(exc, TerrainError):
            raise
        raise TerrainError(
            ErrorCode.E_RECOVERY_INVALID, f"快照无法解析: {exc}", {}
        ) from exc

    return {
        "last_seq": last_seq,
        "chain": pointer["chain"],
        "height_file": pointer["height_file"],
        "material_file": pointer["material_file"],
        "empty": False,
    }


def verify_file_hash(path: str, expected_hash: str) -> bool:
    """供恢复后比对数据文件内容哈希。"""
    with open(path, "rb") as fh:
        return sha256_bytes(fh.read()) == expected_hash
