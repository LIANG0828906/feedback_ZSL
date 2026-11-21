"""Point-cloud workbench core.

The package keeps rendering concerns separate from deterministic editing and
version persistence.  Importing the core does not require an OpenGL context.
"""

from .errors import (
    BusinessError,
    CommitFailed,
    OperatorInvalid,
    PointCloudInvalid,
    SelectionInvalid,
)
from .format import (
    HEADER_STRUCT,
    MAGIC,
    POINT_STRUCT,
    content_hash,
    read_point_cloud,
    serialize_point_cloud,
    write_point_cloud,
)
from .model import Bounds, PointCloud, PointCloudSnapshot

__all__ = [
    "HEADER_STRUCT",
    "MAGIC",
    "POINT_STRUCT",
    "Bounds",
    "BusinessError",
    "CommitFailed",
    "OperatorInvalid",
    "PointCloud",
    "PointCloudInvalid",
    "PointCloudSnapshot",
    "SelectionInvalid",
    "content_hash",
    "read_point_cloud",
    "serialize_point_cloud",
    "write_point_cloud",
]
