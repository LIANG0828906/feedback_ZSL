// Package testsupport contains comparison helpers for deterministic tests.
package testsupport

import (
	"reflect"

	"github.com/liang/collector-backpressure/model"
)

func NormalizeTicks(ticks []model.TickReport) []model.TickReport {
	out := append([]model.TickReport(nil), ticks...)
	for i := range out {
		out[i].Invalidation = nil
		agents := append([]model.AgentReport(nil), out[i].Agents...)
		for j := range agents {
			agents[j].Elapsed = 0
			agents[j].ElapsedNanoseconds = 0
			agents[j].WorkerIndex = 0
		}
		out[i].Agents = agents
		partitions := append([]model.PartitionReport(nil), out[i].Partitions...)
		for j := range partitions {
			partitions[j].ElapsedNanoseconds = 0
			partitions[j].WorkerIndex = 0
		}
		out[i].Partitions = partitions
	}
	return out
}

func EqualSemanticTicks(a, b []model.TickReport) bool {
	return reflect.DeepEqual(NormalizeTicks(a), NormalizeTicks(b))
}
