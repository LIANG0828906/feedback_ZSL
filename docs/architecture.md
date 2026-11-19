# Architecture

## Process layout

```text
scenario / serve
  ├─ orchestration API (:orchestrator_port)
  ├─ load generator (threads)
  └─ 6 × instance supervisor (:instance_port)
       ├─ active version backend (ephemeral loopback port)
       └─ candidate version backend (ephemeral loopback port)
```

The supervisor is the stable process and stable TCP port. A version is never
written over the process currently serving requests. A candidate backend is
started separately, health-checked, and only then installed behind an atomic
proxy target pointer. Requests already holding the old target finish there;
the old backend is stopped after its in-flight count reaches zero.

## Batch state machine

```text
PENDING -> PRECHECKING -> SWITCHING -> OBSERVING -> PASSED
                                             \-> ROLLING_BACK
                                                   ├-> ROLLED_BACK
                                                   ├-> PARTIAL_ROLLBACK
                                                   └-> ROLLBACK_FAILED

Any non-terminal state -> ABORTING -> ABORTED / rollback result
```

`PASSED` and the rollback terminal states are final. A resumed orchestrator
never moves them backwards and never auto-advances a batch that has rolled
back.

## Switching guarantees

1. Every batch member is prechecked before activation starts.
2. A failed precheck leaves that instance on its old version and records the
   stable reason in the batch precheck payload.
3. Activation is per instance. The candidate backend is already warm and
   healthy, and the proxy target changes atomically.
4. The old backend drains before its process exits.
5. A failed candidate start or activation leaves the old active backend in
   place.

Rollback also starts a replacement backend first. If a configured restart
refusal occurs, the rollback action is recorded as failed with
`INSTANCE_RESTART_REFUSED`, the switch marker remains set, and the still-active
version keeps serving. Processing then continues with the remaining switched
instances.

## Transaction boundaries

SQLite uses WAL and `FULL` synchronous mode. Every externally visible state
change is wrapped in `BEGIN IMMEDIATE`:

- batch transition + event
- switched instance marker + event + partial snapshot
- final decision + thresholds + triggers + metrics snapshot + event
- rollback action + instance state + metrics snapshot + event
- manual abort + reason + metrics snapshot + event

The request samples are persisted incrementally so a restarted orchestrator
can continue an in-progress observation. Per-instance sample cursors are
reconstructed from the maximum persisted sample id.

## Determinism

The backend reports a configured simulated latency in
`X-Simulated-Latency-Ms`; the supervisor records that value as the SLI sample
latency. Fault-driven 5xx selection is a deterministic function of the backend
request sequence.

For a batch, each switched instance contributes the first
`quota_per_instance` completed target-version requests from the fixed
observation window. This fixed quota avoids one-request wall-clock jitter
changing the report. Reports contain no timestamps; only fields whose names
end in `_duration_ms` are allowed to vary between identical runs.
