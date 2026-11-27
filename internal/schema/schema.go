// Package schema 实现三个 schema 版本的装载、升降级与损失报告。
// 所有外部表示都被映射为内部 model.RuleSet（tick + 万分比），并保留
// 源版本标识、逐字段转换痕迹、定点截断残差与默认补齐/降级处置记录。
package schema

import (
	"encoding/json"
	"strconv"

	"skillsched/internal/errx"
	"skillsched/internal/fixed"
	"skillsched/internal/model"
)

// 升级时为缺失字段声明的默认值（内部口径）。
const (
	DefaultCDRCapPermyriad int64 = 5000 // v1/v2 无冷却缩减上界，升级默认 50%
)

// FieldConv 记录一次逐字段转换痕迹。
type FieldConv struct {
	Path        string `json:"path"`
	FromVersion string `json:"from_version"`
	FromValue   string `json:"from_value"`
	ToValue     string `json:"to_value"`
	Note        string `json:"note,omitempty"`
}

// DefaultFill 记录低版本升级时的一次默认值补齐。
type DefaultFill struct {
	Path         string `json:"path"`
	OldValue     string `json:"old_value"`
	DefaultValue string `json:"default_value"`
}

// LossItem 记录一次降级无法表达的处置。
type LossItem struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"` // expression_tree / shared_group / cdr_cap
	Disposition string `json:"disposition"`
	Detail      string `json:"detail,omitempty"`
}

// Report 汇总一次装载或导出的全部审计信息。
type Report struct {
	SourceVersion model.Version `json:"source_version"`
	TargetVersion model.Version `json:"target_version,omitempty"`
	Conversions   []FieldConv   `json:"conversions"`
	Truncations   fixed.Trace   `json:"truncations"`
	DefaultFills  []DefaultFill `json:"default_fills"`
	Losses        []LossItem    `json:"losses"`
}

// LoadResult 是装载产物。
type LoadResult struct {
	Rules  *model.RuleSet
	Report *Report
}

func schemaErr(ver model.Version, path string) *errx.Error {
	return errx.New(errx.ESchema, "missing or invalid required field").
		With("source_version", string(ver), "field_path", path)
}

// detectVersion 只读取版本号字段，未知版本返回 E_SCHEMA。
func detectVersion(raw map[string]any) (model.Version, error) {
	for _, k := range []string{"schemaVersion", "schema_version", "SchemaVer"} {
		if v, ok := raw[k]; ok {
			n, ok := v.(float64)
			if !ok {
				return "", errx.New(errx.ESchema, "schema version must be integer").With("field_path", k)
			}
			switch int(n) {
			case 1:
				return model.V1, nil
			case 2:
				return model.V2, nil
			case 3:
				return model.V3, nil
			default:
				return "", errx.New(errx.ESchema, "unknown schema version").
					With("field_path", k, "value", strconv.Itoa(int(n)))
			}
		}
	}
	return "", errx.New(errx.ESchema, "schema version field not found")
}

// Load 按版本号路由装载任意受支持的 schema 字节流。
func Load(data []byte) (*LoadResult, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errx.New(errx.ESchema, "invalid json").Wrap(err)
	}
	ver, err := detectVersion(raw)
	if err != nil {
		return nil, err
	}
	switch ver {
	case model.V1:
		return loadV1(data)
	case model.V2:
		return loadV2(data)
	case model.V3:
		return loadV3(data)
	default:
		return nil, errx.New(errx.ESchema, "unsupported version").With("source_version", string(ver))
	}
}

func requireString(ver model.Version, raw map[string]any, path, key string) (string, error) {
	v, ok := raw[key]
	if !ok {
		return "", schemaErr(ver, path)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", schemaErr(ver, path)
	}
	return s, nil
}

func requireNumber(ver model.Version, raw map[string]any, path, key string) (int64, error) {
	v, ok := raw[key]
	if !ok {
		return 0, schemaErr(ver, path)
	}
	n, ok := v.(float64)
	if !ok {
		return 0, schemaErr(ver, path)
	}
	return int64(n), nil
}

func requireSlice(ver model.Version, raw map[string]any, path, key string) ([]any, error) {
	v, ok := raw[key]
	if !ok {
		return nil, schemaErr(ver, path)
	}
	s, ok := v.([]any)
	if !ok || len(s) == 0 {
		return nil, schemaErr(ver, path)
	}
	return s, nil
}

func child(m map[string]any, key string) map[string]any {
	if v, ok := m[key]; ok {
		if mm, ok := v.(map[string]any); ok {
			return mm
		}
	}
	return nil
}

func msToTicks(rep *Report, path string, ms int64) (int64, error) {
	t, err := fixed.MsToTicks(&rep.Truncations, path, ms, fixed.RoundDown)
	if err != nil {
		return 0, err
	}
	rep.Conversions = append(rep.Conversions, FieldConv{
		Path: path, FromVersion: string(rep.SourceVersion),
		FromValue: strconv.FormatInt(ms, 10) + "ms",
		ToValue:   strconv.FormatInt(t, 10) + "tick",
		Note:      "ms->tick round down",
	})
	return t, nil
}

func permilleToInternal(rep *Report, path string, v int64) (int64, error) {
	out, err := fixed.ToInternal(&rep.Truncations, path, v, fixed.Permille, fixed.RoundDown)
	if err != nil {
		return 0, err
	}
	rep.Conversions = append(rep.Conversions, FieldConv{
		Path: path, FromVersion: string(rep.SourceVersion),
		FromValue: strconv.FormatInt(v, 10) + "/1000",
		ToValue:   strconv.FormatInt(out, 10) + "/10000",
		Note:      "permille->permyriad round down",
	})
	return out, nil
}
