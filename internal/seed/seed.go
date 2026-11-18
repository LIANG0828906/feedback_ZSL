package seed

import (
	"math/rand"
	"sort"

	"configcenter/internal/model"
)

const (
	DefaultNamespace = "orders"
	DefaultSeed      = int64(20261119)
	ConfigItemCount  = 23
)

var orderedKeys = []string{
	"order.timeout_ms",
	"order.retry.max_attempts",
	"order.retry.backoff_ms",
	"order.queue.capacity",
	"order.batch.size",
	"order.batch.flush_ms",
	"order.circuit.failure_threshold",
	"order.circuit.cooldown_ms",
	"order.cache.ttl_ms",
	"order.cache.max_entries",
	"order.rate_limit.qps",
	"order.rate_limit.burst",
	"order.payment.timeout_ms",
	"order.inventory.timeout_ms",
	"order.fulfillment.timeout_ms",
	"order.shipping.timeout_ms",
	"order.worker.count",
	"order.worker.queue_size",
	"order.tracing.sample_rate",
	"order.metrics.interval_ms",
	"order.log.level",
	"order.idempotency.ttl_ms",
	"order.feature.parallel_validation",
}

var baseValues = map[string]string{
	"order.timeout_ms":                  "800",
	"order.retry.max_attempts":          "3",
	"order.retry.backoff_ms":            "100",
	"order.queue.capacity":              "2048",
	"order.batch.size":                  "50",
	"order.batch.flush_ms":              "40",
	"order.circuit.failure_threshold":   "20",
	"order.circuit.cooldown_ms":         "3000",
	"order.cache.ttl_ms":                "1500",
	"order.cache.max_entries":           "4096",
	"order.rate_limit.qps":              "500",
	"order.rate_limit.burst":            "1000",
	"order.payment.timeout_ms":          "900",
	"order.inventory.timeout_ms":        "700",
	"order.fulfillment.timeout_ms":      "1100",
	"order.shipping.timeout_ms":         "850",
	"order.worker.count":                "8",
	"order.worker.queue_size":           "1024",
	"order.tracing.sample_rate":         "10",
	"order.metrics.interval_ms":         "5000",
	"order.log.level":                   "info",
	"order.idempotency.ttl_ms":          "60000",
	"order.feature.parallel_validation": "true",
}

func DefaultOrderConfig() map[string]model.Change {
	result := make(map[string]model.Change, len(orderedKeys))
	for _, key := range orderedKeys {
		result[key] = model.Change{Key: key, Value: baseValues[key]}
	}
	return result
}

// Burst returns all 23 order keys in a deterministic pseudo-random publish
// order. The last value for the timeout key is 1200ms, while no client should
// see any of the intervening per-item versions.
func Burst(round int, fixedSeed int64) []model.Change {
	if fixedSeed == 0 {
		fixedSeed = DefaultSeed
	}
	rng := rand.New(rand.NewSource(fixedSeed + int64(round)*7919))
	permutation := rng.Perm(len(orderedKeys))
	changes := make([]model.Change, 0, len(orderedKeys))
	for _, index := range permutation {
		key := orderedKeys[index]
		changes = append(changes, model.Change{
			Key:   key,
			Value: valueFor(key, round),
		})
	}
	return changes
}

func FinalValues(round int) map[string]string {
	result := make(map[string]string, len(orderedKeys))
	for _, key := range orderedKeys {
		result[key] = valueFor(key, round)
	}
	return result
}

func Keys() []string {
	result := append([]string(nil), orderedKeys...)
	sort.Strings(result)
	return result
}

func valueFor(key string, round int) string {
	if key == "order.timeout_ms" {
		if round <= 1 {
			return "1200"
		}
		return "1200"
	}
	if key == "order.log.level" {
		if round%2 == 0 {
			return "debug"
		}
		return "info"
	}
	if round <= 1 {
		return baseValues[key]
	}
	switch key {
	case "order.retry.max_attempts":
		return "4"
	case "order.retry.backoff_ms":
		return "120"
	case "order.queue.capacity":
		return "4096"
	case "order.batch.size":
		return "64"
	case "order.batch.flush_ms":
		return "30"
	case "order.circuit.failure_threshold":
		return "25"
	case "order.circuit.cooldown_ms":
		return "2500"
	case "order.cache.ttl_ms":
		return "2000"
	case "order.cache.max_entries":
		return "8192"
	case "order.rate_limit.qps":
		return "650"
	case "order.rate_limit.burst":
		return "1300"
	case "order.payment.timeout_ms":
		return "1000"
	case "order.inventory.timeout_ms":
		return "800"
	case "order.fulfillment.timeout_ms":
		return "1300"
	case "order.shipping.timeout_ms":
		return "950"
	case "order.worker.count":
		return "10"
	case "order.worker.queue_size":
		return "2048"
	case "order.tracing.sample_rate":
		return "15"
	case "order.metrics.interval_ms":
		return "3000"
	case "order.idempotency.ttl_ms":
		return "90000"
	case "order.feature.parallel_validation":
		return "false"
	default:
		return baseValues[key]
	}
}
