"""Typed errors shared by storage and HTTP layers."""


class BucketError(Exception):
    status = 400
    code = "bad_request"

    def __init__(self, message: str, **details: object) -> None:
        super().__init__(message)
        self.message = message
        self.details = details


class BadRequest(BucketError):
    status = 400
    code = "bad_request"


class NotFound(BucketError):
    status = 404
    code = "not_found"


class Gone(BucketError):
    status = 410
    code = "revision_reclaimed"


class Conflict(BucketError):
    status = 409
    code = "conflict"


class PayloadTooLarge(BucketError):
    status = 413
    code = "payload_too_large"


class UpstreamError(BucketError):
    status = 502
    code = "upstream_error"


class SyncIncomplete(BucketError):
    status = 409
    code = "sync_incomplete"

