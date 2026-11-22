"""Content hashes and the ordered append-only publication chain."""

from __future__ import annotations

from copy import deepcopy
from typing import Any, Iterable

from .canonical import payload_hash, sha256_value
from .errors import LCFTError

GENESIS = "0" * 64


def seal_item(item: dict[str, Any]) -> dict[str, Any]:
    sealed = deepcopy(item)
    sealed["content_hash"] = payload_hash(sealed)
    return sealed


def build_chain(items: Iterable[tuple[str, dict[str, Any]]]) -> list[dict[str, Any]]:
    chain: list[dict[str, Any]] = []
    previous = GENESIS
    for label, item in items:
        payload = item["content_hash"]
        link_hash = sha256_value(
            {
                "label": label,
                "id": item["id"],
                "payload_hash": payload,
                "prev": previous,
            }
        )
        chain.append(
            {
                "label": label,
                "id": item["id"],
                "payload_hash": payload,
                "prev": previous,
                "hash": link_hash,
            }
        )
        previous = link_hash
    return chain


def validate_chain(
    chain: Any,
    report: dict[str, Any],
) -> None:
    if not isinstance(chain, list):
        raise LCFTError(
            "E_RECOVERY_INVALID",
            "snapshot hash chain is missing or not an array",
            field="snapshot.chain",
        )
    expected_items: list[tuple[str, dict[str, Any]]] = []
    for label, key in (
        ("sentence", "sentences"),
        ("description", "descriptions"),
        ("mark", "speaker_marks"),
    ):
        for index, item in enumerate(report.get(key, [])):
            if not isinstance(item, dict):
                raise LCFTError(
                    "E_RECOVERY_INVALID",
                    f"{key} contains a non-object entry",
                    field=f"snapshot.{key}[{index}]",
                    details={
                        "link_index": index,
                        "first_broken_link": {
                            "id": None,
                            "reason": "invalid_type",
                        },
                    },
                )
            expected_items.append((label, item))
    if len(chain) != len(expected_items):
        index = min(len(chain), len(expected_items))
        expected = expected_items[index][1]["id"] if index < len(expected_items) else None
        raise LCFTError(
            "E_RECOVERY_INVALID",
            "hash chain length does not match published entities",
            field=f"snapshot.chain[{index}]",
            details={
                "link_index": index,
                "first_broken_link": {"id": expected, "reason": "length_mismatch"},
                "expected_links": len(expected_items),
                "actual_links": len(chain),
            },
        )
    previous = GENESIS
    for index, ((label, item), link) in enumerate(zip(expected_items, chain)):
        identifier = item.get("id")
        broken = False
        reason = ""
        if not isinstance(link, dict):
            broken, reason = True, "link_not_object"
        elif any(key not in link for key in ("label", "id", "payload_hash", "prev", "hash")):
            broken, reason = True, "missing_field"
        elif link.get("label") != label or link.get("id") != identifier:
            broken, reason = True, "identity_mismatch"
        else:
            expected_payload = item.get("content_hash")
            if expected_payload != payload_hash(item):
                broken, reason = True, "payload_hash_mismatch"
            elif link.get("payload_hash") != expected_payload:
                broken, reason = True, "chain_payload_mismatch"
            elif link.get("prev") != previous:
                broken, reason = True, "prev_mismatch"
            else:
                expected_link_hash = sha256_value(
                    {
                        "label": label,
                        "id": identifier,
                        "payload_hash": expected_payload,
                        "prev": previous,
                    }
                )
                if link.get("hash") != expected_link_hash:
                    broken, reason = True, "link_hash_mismatch"
        if broken:
            raise LCFTError(
                "E_RECOVERY_INVALID",
                f"hash chain is broken at link {index}",
                field=f"snapshot.chain[{index}].hash",
                details={
                    "link_index": index,
                    "first_broken_link": {"id": identifier, "reason": reason},
                },
            )
        previous = link["hash"]


def report_hash(report: dict[str, Any]) -> str:
    return sha256_value(
        {key: value for key, value in report.items() if key not in ("track_hash", "chain")}
    )

