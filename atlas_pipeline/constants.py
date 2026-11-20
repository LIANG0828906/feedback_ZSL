from __future__ import annotations

from .errors import AtlasError, ErrorCode

ATLAS_MAGIC = b"ATLSHDR1"
ATLAS_FORMAT_VERSION = 1
HEADER_SIZE = 48
STORAGE_FLOAT16_RGBA = 1
CHANNELS = 4

COLOR_SPACE_TO_CODE = {
    "rec709": 0,
    "display-p3": 1,
    "rec2020": 2,
}
CODE_TO_COLOR_SPACE = {value: key for key, value in COLOR_SPACE_TO_CODE.items()}

TRANSFER_TO_CODE = {
    "linear": 0,
    "srgb": 1,
    "pq": 2,
}
CODE_TO_TRANSFER = {value: key for key, value in TRANSFER_TO_CODE.items()}

TONE_CURVES = ("none", "reinhard", "hable")
TONE_CURVES_WITH_COMPRESSION = frozenset(("reinhard", "hable"))
WHITE_POINTS = ("D50", "D65")
ADAPTATION_METHODS = ("bradford", "none")
BIT_DEPTHS = (8, 10, 12, 16)


def require_color_space(value: str, *, field: str = "color_space") -> str:
    if value not in COLOR_SPACE_TO_CODE:
        raise AtlasError(
            ErrorCode.E_ATLAS_INVALID,
            f"unknown color space {value!r}; expected one of {sorted(COLOR_SPACE_TO_CODE)}",
            field=field,
        )
    return value


def require_transfer(value: str, *, field: str = "transfer_function") -> str:
    if value not in TRANSFER_TO_CODE:
        raise AtlasError(
            ErrorCode.E_ATLAS_INVALID,
            f"unknown transfer function {value!r}; expected one of {sorted(TRANSFER_TO_CODE)}",
            field=field,
        )
    return value


def parse_color_code(code: int) -> str:
    try:
        return CODE_TO_COLOR_SPACE[int(code)]
    except (KeyError, ValueError):
        raise AtlasError(
            ErrorCode.E_ATLAS_INVALID,
            f"unknown color space code {code}",
            field="color_space_code",
            details={"value": int(code)},
        ) from None


def parse_transfer_code(code: int) -> str:
    try:
        return CODE_TO_TRANSFER[int(code)]
    except (KeyError, ValueError):
        raise AtlasError(
            ErrorCode.E_ATLAS_INVALID,
            f"unknown transfer function code {code}",
            field="transfer_function_code",
            details={"value": int(code)},
        ) from None
