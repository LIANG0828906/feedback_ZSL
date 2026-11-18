from __future__ import annotations

from typing import Any


FAULT_SYN_NOT_REACHED_UPSTREAM = "syn_not_reached_upstream"
FAULT_UPSTREAM_EARLY_DISCONNECT = "upstream_early_disconnect"
FAULT_PROXY_READ_TIMEOUT = "proxy_read_timeout"
FAULT_UNEXPLAINED_RESET = "unexplained_reset"
FAULT_UNEXPLAINED_TIMEOUT = "unexplained_timeout"

KNOWN_FAULTS = (
    FAULT_SYN_NOT_REACHED_UPSTREAM,
    FAULT_UPSTREAM_EARLY_DISCONNECT,
    FAULT_PROXY_READ_TIMEOUT,
)

ABNORMAL_KINDS = {
    "syn_dropped",
    "connect_fail",
    "connect_timeout",
    "reset",
    "timeout",
    "write_failed",
}


def _format_event(event: dict[str, Any] | None) -> dict[str, Any] | None:
    if event is None:
        return None
    return {
        "connection_id": event["connection_id"],
        "details": event.get("details", {}),
        "event_id": event["event_id"],
        "hop": event["hop"],
        "initiator": event["initiator"],
        "kind": event["kind"],
        "ts": event["ts"],
    }


def build_coverage(
    events: list[dict[str, Any]],
    request_id: str,
    outcome: str,
) -> tuple[dict[str, Any], list[str], list[dict[str, str]]]:
    coverage: dict[str, Any] = {}
    missing_hops: list[str] = []
    missing_events: list[dict[str, str]] = []
    required = {
        "client": ("request_sent",),
        "proxy": ("request_received",),
        "upstream": ("request_received",),
    }
    for hop in ("client", "proxy", "upstream"):
        hop_events = [event for event in events if event["hop"] == hop]
        request_events = [
            event for event in hop_events if event["request_id"] == request_id
        ]
        kinds = {event["kind"] for event in request_events}
        if not hop_events:
            status = "missing_hop"
        elif not request_events:
            status = "missing_request_event"
        elif any(kind not in kinds for kind in required[hop]):
            status = "missing_required_event"
        else:
            status = "observed"
        if status != "observed":
            missing_hops.append(hop)
        for required_kind in required[hop]:
            if required_kind not in kinds:
                missing_events.append({"event": required_kind, "hop": hop})
        coverage[hop] = {
            "event_count": len(hop_events),
            "observed": bool(hop_events),
            "request_event_count": len(request_events),
            "request_observed": bool(request_events),
            "status": status,
        }
    return coverage, missing_hops, missing_events


def attribute(
    *,
    request_id: str,
    outcome: str,
    events: list[dict[str, Any]],
) -> dict[str, Any]:
    if outcome not in ("reset", "timeout"):
        return {
            "edge": None,
            "evidence": None,
            "evidence_event_id": None,
            "first_abnormal_event": None,
            "fault_type": None,
            "hop": None,
            "rationale": "request completed without reset or timeout semantics",
            "reuse_failure": False,
            "subtype": None,
        }
    abnormal = [
        event
        for event in events
        if event["kind"] in ABNORMAL_KINDS
        or event["kind"] == "reuse_failure"
        or (
            event["kind"] == "close"
            and event.get("details", {}).get("unexpected", False)
        )
    ]
    first_abnormal = abnormal[0] if abnormal else None
    reuse_failure_event = next(
        (event for event in events if event["kind"] == "reuse_failure"), None
    )
    reuse_failure = reuse_failure_event is not None or any(
        event.get("details", {}).get("reuse_failure", False) for event in events
    )

    fault_type: str | None = None
    attribution_hop: str | None = None
    edge: str | None = None
    evidence: dict[str, Any] | None = None
    rationale = ""
    subtype: str | None = None

    syn_events = [event for event in events if event["kind"] == "syn_dropped"]
    proxy_timeouts = [
        event
        for event in events
        if event["hop"] == "proxy"
        and event["kind"] in ("timeout", "read_timeout")
        and (
            event.get("details", {}).get("fault_class") == FAULT_PROXY_READ_TIMEOUT
            or event.get("details", {}).get("phase") == "upstream_response_head"
        )
    ]
    upstream_abnormal = [
        event
        for event in events
        if event["hop"] == "upstream"
        and event["kind"] in ("reset", "write_failed", "close")
    ]

    if syn_events:
        event = syn_events[0]
        fault_type = FAULT_SYN_NOT_REACHED_UPSTREAM
        attribution_hop = event["hop"]
        edge = "proxy_to_upstream"
        evidence = event
        rationale = (
            "proxy emitted syn_dropped with syn_ack_received=false and no upstream "
            "connection/request event exists"
        )
    elif proxy_timeouts:
        event = proxy_timeouts[0]
        fault_type = FAULT_PROXY_READ_TIMEOUT
        attribution_hop = event["hop"]
        edge = "proxy_to_upstream"
        evidence = event
        rationale = (
            "proxy read timer expired while awaiting the upstream response; the "
            "upstream request event is available but no upstream response event exists"
        )
    elif upstream_abnormal or reuse_failure:
        event = upstream_abnormal[0] if upstream_abnormal else reuse_failure_event
        fault_type = FAULT_UPSTREAM_EARLY_DISCONNECT
        attribution_hop = "upstream"
        edge = "proxy_to_upstream"
        evidence = event
        subtype = "reuse_failed_connection" if reuse_failure else None
        rationale = (
            "upstream terminated or reset the connection before a valid response; "
            "the proxy had selected a previously pooled connection"
            if reuse_failure and event is not None and event["hop"] == "proxy"
            else "upstream emitted reset/write_failed before a valid response"
        )
    elif outcome == "reset":
        fault_type = FAULT_UNEXPLAINED_RESET
        attribution_hop = first_abnormal["hop"] if first_abnormal else "unknown"
        edge = "unknown"
        evidence = first_abnormal
        rationale = (
            "client observed a reset but the event sequence does not match SYN loss, "
            "upstream early disconnect, or proxy read timeout"
        )
    elif outcome == "timeout":
        fault_type = FAULT_UNEXPLAINED_TIMEOUT
        attribution_hop = first_abnormal["hop"] if first_abnormal else "unknown"
        edge = "unknown"
        evidence = first_abnormal
        rationale = (
            "request exceeded its deadline without a proxy read-timeout classification"
        )

    return {
        "edge": edge,
        "evidence": _format_event(evidence),
        "evidence_event_id": evidence["event_id"] if evidence else None,
        "first_abnormal_event": _format_event(first_abnormal),
        "fault_type": fault_type,
        "hop": attribution_hop,
        "rationale": rationale,
        "reuse_failure": reuse_failure,
        "subtype": subtype,
    }
