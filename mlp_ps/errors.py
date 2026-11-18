from __future__ import annotations

from typing import Any, Dict, Mapping, Optional


E_LAYOUT_CONFLICT = "E_LAYOUT_CONFLICT"
E_PARAM_SHAPE = "E_PARAM_SHAPE"
E_CKPT_MISSING = "E_CKPT_MISSING"
E_CKPT_CORRUPT = "E_CKPT_CORRUPT"
E_PROGRESS_RANGE = "E_PROGRESS_RANGE"
E_TRAIN_STALL = "E_TRAIN_STALL"
E_REGISTRATION_TIMEOUT = "E_REGISTRATION_TIMEOUT"
E_PROTOCOL = "E_PROTOCOL"
E_CONFIG = "E_CONFIG"
E_NETWORK = "E_NETWORK"
E_INTERNAL = "E_INTERNAL"


EXIT_CODES = {
    "OK": 0,
    E_CONFIG: 2,
    E_LAYOUT_CONFLICT: 10,
    E_PARAM_SHAPE: 11,
    E_CKPT_MISSING: 12,
    E_CKPT_CORRUPT: 13,
    E_PROGRESS_RANGE: 14,
    E_TRAIN_STALL: 15,
    E_REGISTRATION_TIMEOUT: 16,
    E_PROTOCOL: 17,
    E_NETWORK: 18,
    E_INTERNAL: 19,
}


class BusinessError(RuntimeError):
    def __init__(
        self,
        code: str,
        message: str,
        details: Optional[Mapping[str, Any]] = None,
    ) -> None:
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.details: Dict[str, Any] = dict(details or {})

    @property
    def exit_code(self) -> int:
        return EXIT_CODES.get(self.code, EXIT_CODES[E_INTERNAL])

    def to_dict(self) -> Dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "details": self.details,
            "exit_code": self.exit_code,
        }


def exit_code_for(code: str) -> int:
    return EXIT_CODES.get(code, EXIT_CODES[E_INTERNAL])
