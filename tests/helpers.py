from __future__ import annotations

import numpy as np

from pcwb.model import PointCloud, PointCloudSnapshot
from pcwb.selection import Selection


def sample_cloud(count: int = 10) -> PointCloud:
    x = np.linspace(-1.0, 1.0, count, dtype=np.float32)
    xyz = np.column_stack(
        [
            x,
            np.sin(x * 2.0).astype(np.float32),
            (x * 0.25).astype(np.float32),
        ]
    )
    rgb = np.column_stack(
        [
            np.arange(count, dtype=np.uint8),
            np.arange(count, dtype=np.uint8) * 3,
            np.arange(count, dtype=np.uint8) * 7,
        ]
    ).astype(np.uint8)
    return PointCloud.from_arrays(xyz, rgb)


def snapshot(cloud: PointCloud | None = None, version: int = 1) -> PointCloudSnapshot:
    value = cloud or sample_cloud()
    return PointCloudSnapshot(
        version=version,
        content_hash=value.content_hash,
        cloud=value,
        parent_version=max(0, version - 1),
    )


def all_selection(cloud: PointCloud, version: int = 1) -> Selection:
    return Selection.from_indices(snapshot(cloud, version), np.arange(cloud.count))
