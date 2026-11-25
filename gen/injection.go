package gen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

func InjectNegativeQuota(v2 []byte) ([]byte, map[string]any, error) {
	return mutateV2(v2, func(root map[string]any) error {
		tenants, ok := root["tenants"].([]any)
		if !ok || len(tenants) == 0 {
			return fmt.Errorf("missing tenants")
		}
		tenant, ok := tenants[0].(map[string]any)
		if !ok {
			return fmt.Errorf("bad tenant")
		}
		tenant["quota_basis_points"] = -1
		return nil
	})
}

func InjectQuotaScaleOverflow(v2 []byte) ([]byte, map[string]any, error) {
	return mutateV2(v2, func(root map[string]any) error {
		tenants, ok := root["tenants"].([]any)
		if !ok || len(tenants) == 0 {
			return fmt.Errorf("missing tenants")
		}
		tenant, ok := tenants[0].(map[string]any)
		if !ok {
			return fmt.Errorf("bad tenant")
		}
		tenant["quota_basis_points"] = 10001
		return nil
	})
}

func InjectGuaranteeOverCapacity(v2 []byte) ([]byte, map[string]any, error) {
	return mutateV2(v2, func(root map[string]any) error {
		tenants, ok := root["tenants"].([]any)
		if !ok || len(tenants) < 4 {
			return fmt.Errorf("missing tenants")
		}
		guarantees := []int{6000, 5000, 3000, 3000}
		for i, raw := range tenants {
			tenant, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("bad tenant")
			}
			tenant["quota_basis_points"] = 10000
			tenant["guarantee_basis_points"] = guarantees[i]
		}
		return nil
	})
}

func InjectUnknownSchema(v2 []byte) ([]byte, map[string]any, error) {
	return mutateV2(v2, func(root map[string]any) error {
		root["schema_version"] = "v9"
		return nil
	})
}

func mutateV2(v2 []byte, mutate func(map[string]any) error) ([]byte, map[string]any, error) {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(v2))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, nil, err
	}
	if err := mutate(root); err != nil {
		return nil, nil, err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return out, root, nil
}

// TruncateCopy writes a partial copy of a checkpoint file and leaves the
// original untouched.
func TruncateCopy(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if len(raw) < 2 {
		return fmt.Errorf("checkpoint too small to truncate")
	}
	return os.WriteFile(dst, raw[:len(raw)/2], 0o600)
}
