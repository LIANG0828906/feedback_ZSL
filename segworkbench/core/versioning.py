"""算法版本与参数版本。

任何会改变掩码像素结果或几何统计口径的实现改动，都必须提升
``ALGORITHM_VERSION``；参数结构变更提升 ``PARAM_SCHEMA_VERSION``。
检查点同时记录两者，恢复时任一不匹配则整份快照作废。
"""
from __future__ import annotations

ALGORITHM_VERSION = "alg-1.0.0"
PARAM_SCHEMA_VERSION = "param-1.0.0"
SNAPSHOT_FORMAT_VERSION = "snapshot-1.0.0"
