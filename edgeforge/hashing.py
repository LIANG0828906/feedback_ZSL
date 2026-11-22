from __future__ import annotations

import hashlib
import sys

import numpy as np


def canonical_array_bytes(array: np.ndarray) -> bytes:
    contiguous = np.ascontiguousarray(array)
    header = (
        f"{contiguous.dtype.str}|{contiguous.shape}|C|"
        f"{int(sys.byteorder == 'little')}".encode("ascii")
    )
    return header + contiguous.tobytes(order="C")


def sha256_array(array: np.ndarray) -> str:
    return hashlib.sha256(canonical_array_bytes(array)).hexdigest()


def sha256_file(path: str) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()

