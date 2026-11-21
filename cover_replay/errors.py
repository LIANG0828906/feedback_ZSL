"""Business error types and stable error codes."""

from __future__ import annotations

from typing import Any


class BusinessError(Exception):
    """An error that is safe to serialize as a machine-readable repair result."""

    def __init__(self, code: str, message: str, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = details

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
        }
        payload.update(self.details)
        return payload

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"


class RecordInvalid(BusinessError):
    def __init__(
        self,
        message: str,
        *,
        shot_index: int | None = None,
        field: str | None = None,
        block_id: int | None = None,
    ) -> None:
        details: dict[str, Any] = {}
        if shot_index is not None:
            details["shot_index"] = shot_index
        if field is not None:
            details["field"] = field
        if block_id is not None:
            details["block_id"] = block_id
        super().__init__("E_RECORD_INVALID", message, **details)


class ConfigInvalid(BusinessError):
    def __init__(self, message: str, *, field: str | None = None) -> None:
        details = {"field": field} if field else {}
        super().__init__("E_CONFIG_INVALID", message, **details)


class BusyError(BusinessError):
    def __init__(self, step: int) -> None:
        super().__init__(
            "E_BUSY",
            f"physical step {step} has already been submitted",
            step=step,
        )


class MemoryBudgetError(BusinessError):
    def __init__(self, step: int, block_id: int, required: int, budget: int) -> None:
        super().__init__(
            "E_MEMORY_BUDGET",
            "a cover block's memory footprint exceeds the configured step budget",
            step=step,
            block_id=block_id,
            required=required,
            budget=budget,
        )


class CreditConflictError(BusinessError):
    def __init__(
        self,
        *,
        step: int,
        block_id: int,
        shot_index: int,
        predicted_remaining: str,
        authoritative_remaining: str,
        client_sequence: int,
        player_id: str,
    ) -> None:
        super().__init__(
            "E_CREDIT_CONFLICT",
            "client predicted durability differs from authoritative settlement",
            step=step,
            block_id=block_id,
            shot_index=shot_index,
            client_sequence=client_sequence,
            player_id=player_id,
            predicted_remaining=predicted_remaining,
            authoritative_remaining=authoritative_remaining,
        )


class RecoveryInvalidError(BusinessError):
    def __init__(self, step: int, message: str, **details: Any) -> None:
        super().__init__(
            "E_RECOVERY_INVALID",
            message,
            step=step,
            first_broken_step=step,
            **details,
        )


class CrackUnstableError(BusinessError):
    def __init__(
        self,
        *,
        step: int,
        block_ids: list[int],
        propagation_chain: list[dict[str, int]],
        iterations: int,
    ) -> None:
        super().__init__(
            "E_CRACK_UNSTABLE",
            "crack propagation did not converge within the configured iteration limit",
            step=step,
            block_ids=block_ids,
            propagation_chain=propagation_chain,
            iterations=iterations,
        )


class BaselineVersionError(BusinessError):
    def __init__(
        self,
        *,
        baseline_version: str | None,
        expected_baseline_version: str,
        model_version: str | None,
        expected_model_version: str,
    ) -> None:
        super().__init__(
            "E_BASELINE_VERSION",
            "baseline or damage model version does not match this replay",
            baseline_version=baseline_version,
            expected_baseline_version=expected_baseline_version,
            model_version=model_version,
            expected_model_version=expected_model_version,
        )


class BaselineDivergenceError(BusinessError):
    def __init__(
        self,
        *,
        step: int,
        field_path: str,
        baseline_value: Any,
        replay_value: Any,
        effective_client_sequences: list[int],
        queue_watermark: int | None,
        memory_budget: int | None,
        attribution: str | None = None,
    ) -> None:
        extra = {"attribution": attribution} if attribution else {}
        super().__init__(
            "E_BASELINE_DIVERGENCE",
            "replay state diverges from the authoritative baseline",
            step=step,
            first_mismatch_step=step,
            field_path=field_path,
            baseline_value=baseline_value,
            replay_value=replay_value,
            effective_client_sequences=effective_client_sequences,
            queue_watermark=queue_watermark,
            memory_budget=memory_budget,
            **extra,
        )


class StrategyDivergenceError(BusinessError):
    def __init__(self, details: dict[str, Any]) -> None:
        super().__init__(
            "E_STRATEGY_DIVERGENCE",
            "full-scan and windowed settlement produced different results",
            **details,
        )
