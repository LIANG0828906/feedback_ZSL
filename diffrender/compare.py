"""Exact run comparison and first-difference localization."""

from __future__ import annotations

import math
from typing import Any

from .errors import AppError


def _difference(
    *,
    category: str,
    field_path: str,
    reference: Any,
    candidate: Any,
    identifier: str | None = None,
    index: int | None = None,
) -> dict[str, Any]:
    result: dict[str, Any] = {
        "category": category,
        "field_path": field_path,
        "reference": reference,
        "candidate": candidate,
    }
    if identifier is not None:
        result["parameter_identifier"] = identifier
    if index is not None:
        result["round_or_index"] = index
    if (
        isinstance(reference, (int, float))
        and isinstance(candidate, (int, float))
        and math.isfinite(float(reference))
        and math.isfinite(float(candidate))
    ):
        result["absolute_difference"] = abs(float(reference) - float(candidate))
    return result


def compare_reports(
    reference: dict[str, Any],
    candidate: dict[str, Any],
    *,
    compare_gradient_history: bool = False,
) -> dict[str, Any]:
    differences: list[dict[str, Any]] = []
    reference_params = reference.get("final_parameters", {})
    candidate_params = candidate.get("final_parameters", {})
    for label, value in reference_params.items():
        if label not in candidate_params:
            differences.append(
                _difference(
                    category="parameter",
                    identifier=label,
                    field_path=f"final_parameters.{label}",
                    reference=value,
                    candidate="<missing>",
                )
            )
        elif candidate_params[label] != value:
            differences.append(
                _difference(
                    category="parameter",
                    identifier=label,
                    field_path=f"final_parameters.{label}",
                    reference=value,
                    candidate=candidate_params[label],
                )
            )
    for label in candidate_params:
        if label not in reference_params:
            differences.append(
                _difference(
                    category="parameter",
                    identifier=label,
                    field_path=f"final_parameters.{label}",
                    reference="<missing>",
                    candidate=candidate_params[label],
                )
            )
    reference_losses = list(reference.get("loss_history", []))
    candidate_losses = list(candidate.get("loss_history", []))
    for index in range(max(len(reference_losses), len(candidate_losses))):
        ref = reference_losses[index] if index < len(reference_losses) else "<missing>"
        cand = (
            candidate_losses[index]
            if index < len(candidate_losses)
            else "<missing>"
        )
        if ref != cand:
            differences.append(
                _difference(
                    category="loss",
                    field_path=f"loss_history[{index}]",
                    index=index,
                    reference=ref,
                    candidate=cand,
                )
            )
    reference_submissions = list(reference.get("submission_sequence", []))
    candidate_submissions = list(candidate.get("submission_sequence", []))
    for index in range(max(len(reference_submissions), len(candidate_submissions))):
        ref = (
            reference_submissions[index]
            if index < len(reference_submissions)
            else "<missing>"
        )
        cand = (
            candidate_submissions[index]
            if index < len(candidate_submissions)
            else "<missing>"
        )
        if ref != cand:
            differences.append(
                _difference(
                    category="block_submission",
                    field_path=f"submission_sequence[{index}]",
                    index=index,
                    reference=ref,
                    candidate=cand,
                )
            )
    scalar_fields = (
        "final_loss",
        "iterations_completed",
        "parameter_version",
        "gradient_clip_events",
    )
    for field_name in scalar_fields:
        ref = reference.get(field_name)
        cand = candidate.get(field_name)
        if ref != cand:
            differences.append(
                _difference(
                    category="scalar",
                    field_path=field_name,
                    reference=ref,
                    candidate=cand,
                )
            )
    if compare_gradient_history:
        reference_gradients = list(reference.get("gradient_history", []))
        candidate_gradients = list(candidate.get("gradient_history", []))
        for index in range(max(len(reference_gradients), len(candidate_gradients))):
            ref = (
                reference_gradients[index]
                if index < len(reference_gradients)
                else "<missing>"
            )
            cand = (
                candidate_gradients[index]
                if index < len(candidate_gradients)
                else "<missing>"
            )
            if ref != cand:
                differences.append(
                    _difference(
                        category="gradient",
                        field_path=f"gradient_history[{index}]",
                        index=index,
                        reference=ref,
                        candidate=cand,
                    )
                )
                break
    return {
        "ok": not differences,
        "equal": not differences,
        "difference_count": len(differences),
        "first_difference": differences[0] if differences else None,
        "differences": differences,
        "compared": {
            "parameters": len(reference_params),
            "loss_rounds": max(len(reference_losses), len(candidate_losses)),
            "block_submissions": max(
                len(reference_submissions), len(candidate_submissions)
            ),
            "gradient_history": compare_gradient_history,
        },
    }


def assert_reports_equal(
    reference: dict[str, Any],
    candidate: dict[str, Any],
    *,
    compare_gradient_history: bool = False,
    label: str = "run",
) -> dict[str, Any]:
    report = compare_reports(
        reference,
        candidate,
        compare_gradient_history=compare_gradient_history,
    )
    if not report["equal"]:
        first = report["first_difference"]
        raise AppError(
            "E_DIFF",
            f"{label} differs from the reference run",
            first["field_path"],
            report,
        )
    return report

