"""Stable symbolic error codes used by the workbench API."""

E_POINTCLOUD_INVALID = "E_POINTCLOUD_INVALID"
E_COMMIT_FAILED = "E_COMMIT_FAILED"
E_SELECTION_STALE = "E_SELECTION_STALE"
E_UNKNOWN_VERSION = "E_UNKNOWN_VERSION"
E_CANCELLED = "E_CANCELLED"


class PointCloudError(Exception):
    """Exception carrying one of the stable symbolic error codes."""

    def __init__(self, code: str, message: str = ""):
        super().__init__(f"{code}: {message}" if message else code)
        self.code = code
        self.message = message
