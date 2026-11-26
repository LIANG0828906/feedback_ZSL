package loader

import (
	"schedlab/internal/faults"
	"schedlab/internal/scale"
)

func parseCores(token, ctx string) (int64, error) {
	v, _, err := scale.CoresToMilli(token)
	if err != nil {
		if err == scale.ErrOverflow {
			return 0, faults.Wrap(faults.E_OVERFLOW, "core conversion overflow", err, "field", ctx, "value", token)
		}
		return 0, faults.Wrap(faults.E_RESOURCE_PARSE, "bad core token", err, "field", ctx, "value", token)
	}
	return v, nil
}

func parseMemory(token, ctx string) (int64, error) {
	v, _, err := scale.MemoryToBytes(token)
	if err != nil {
		if err == scale.ErrOverflow {
			return 0, faults.Wrap(faults.E_OVERFLOW, "memory conversion overflow", err, "field", ctx, "value", token)
		}
		return 0, faults.Wrap(faults.E_RESOURCE_PARSE, "bad memory token", err, "field", ctx, "value", token)
	}
	return v, nil
}

func cloneLabels(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func semList(m map[string]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = []string{v}
	}
	return out
}
