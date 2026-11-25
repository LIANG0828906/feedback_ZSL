// Package qos implements an embeddable, deterministic egress traffic-shaping
// pipeline for edge gateways.
//
// It loads qos.v1, qos.v2 and qos.v3 project documents into one normalized
// channel model, advances integer token buckets per logical tick, arbitrates a
// shared egress budget by priority and weight, and maintains an ordered delay
// queue with deterministic replay. Incremental edits reuse prefix snapshots and
// report their invalidation closure. SQLite checkpoints use a strict
// channel/queue-before-clock write order and a project-version/digest gate.
//
// All fixed-point conversions use checked integer arithmetic. The canonical
// rate is 1/10000 byte per millisecond, and token refill carries an exact
// remainder so long-running tick sequences do not accumulate conversion drift.
package qos
