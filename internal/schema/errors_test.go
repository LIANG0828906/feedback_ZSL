package schema_test

import (
	"testing"

	"skillsched/internal/errx"
	"skillsched/internal/schema"
)

func TestV1MissingFieldESchema(t *testing.T) {
	// v1 缺少 skills[0].cooldownMs。
	raw := `{
	 "schemaVersion":1,
	 "params":{"paramVersion":1,"tickMs":100,"horizonMs":800,"windowMs":800},
	 "pools":[{"poolId":"p","poolName":"p","maxAmount":100,"regenTable":{"const":1,"terms":[]}}],
	 "skills":[{"skillId":"s","skillName":"s","usePool":"p","resourceCost":10,"cooldownReducePermille":100,"orderWeight":1}]
	}`
	_, err := schema.Load([]byte(raw))
	if errx.CodeOf(err) != errx.ESchema {
		t.Fatalf("want E_SCHEMA got %v", err)
	}
	var de *errx.Error
	if !errxAs(err, &de) || de.Fields["source_version"] != "v1" || de.Fields["field_path"] == "" {
		t.Fatalf("missing context: %v", err)
	}
}

func TestV2MissingFieldESchema(t *testing.T) {
	raw := `{
	 "schema_version":2,
	 "params":{"param_version":1,"tick_ms":100,"horizon_ms":800,"window_ms":800},
	 "pools":[{"pool_id":"p","pool_name":"p","max_amount":100,"regen_table":{"const":1,"terms":[]}}],
	 "skills":[{"skill_id":"s","skill_name":"s","use_pool":"p","resource_cost":10,"cooldown_ms":200,"order_weight":1}]
	}`
	_, err := schema.Load([]byte(raw))
	if errx.CodeOf(err) != errx.ESchema {
		t.Fatalf("want E_SCHEMA got %v", err)
	}
}

func TestInvalidJSONIsSchema(t *testing.T) {
	if _, err := schema.Load([]byte("{not-json")); errx.CodeOf(err) != errx.ESchema {
		t.Fatalf("want E_SCHEMA got %v", err)
	}
}
