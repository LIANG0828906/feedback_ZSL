"""Structured error codes and exit codes for the willow lantern renderer."""

EXIT_OK = 0
EXIT_UNEXPECTED = 1
EXIT_VALIDATION = 2
EXIT_SCENE_IO = 3
EXIT_VERIFY_MISMATCH = 4
EXIT_SNAPSHOT_CORRUPT = 5


class RendererError(Exception):
    """Base error carrying a structured business code."""

    code = "INTERNAL_ERROR"
    exit_code = EXIT_UNEXPECTED

    def __init__(self, message, *, layer_id=None, field=None, code=None, exit_code=None):
        super().__init__(message)
        self.message = message
        self.layer_id = layer_id
        self.field = field
        if code is not None:
            self.code = code
        if exit_code is not None:
            self.exit_code = exit_code

    def to_dict(self):
        return {
            "error": {
                "code": self.code,
                "message": self.message,
                "layer_id": self.layer_id,
                "field": self.field,
            }
        }


class SceneIOError(RendererError):
    code = "SCENE_IO_ERROR"
    exit_code = EXIT_SCENE_IO


class SceneParseError(RendererError):
    code = "SCENE_PARSE_ERROR"
    exit_code = EXIT_SCENE_IO


class ValidationError(RendererError):
    exit_code = EXIT_VALIDATION

    def __init__(self, message, *, code, layer_id=None, field=None):
        super().__init__(message, code=code, layer_id=layer_id, field=field)


class SnapshotCorruptError(RendererError):
    code = "SNAPSHOT_CORRUPT"
    exit_code = EXIT_SNAPSHOT_CORRUPT


class VerifyMismatchError(RendererError):
    code = "VERIFY_MISMATCH"
    exit_code = EXIT_VERIFY_MISMATCH

    def __init__(self, message, *, diff):
        super().__init__(message)
        self.diff = diff

    def to_dict(self):
        payload = super().to_dict()
        payload["error"]["diff"] = self.diff
        return payload
