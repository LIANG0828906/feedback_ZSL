package errors

import "fmt"

// 业务错误码，全部为确定性可判定的字符串常量。
const (
	ESchema   = "E_SCHEMA"  // 字段缺失 / 多边形不闭合等结构问题
	ERef      = "E_REF"     // 引用不存在的区域
	EGeo      = "E_GEO"     // 定点精度参数越界
	EDegen    = "E_DEGEN"   // 墙体端点退化（重合）
	EPath     = "E_PATH"    // 绕射路径不存在
	EDiff     = "E_DIFF"    // 增量与全量结果不一致
	EStorage  = "E_STORAGE" // SQLite 持久化故障
	EInternal = "E_INTERNAL"
)

// WorkError 携带错误码与定位字段。
type WorkError struct {
	Code   string
	Field  string
	Detail string
}

func (e *WorkError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s field=%s: %s", e.Code, e.Field, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

// New 构造一个 WorkError。
func New(code, field, detail string) *WorkError {
	return &WorkError{Code: code, Field: field, Detail: detail}
}

// CodeOf 提取错误码，非 WorkError 统一归为 E_INTERNAL。
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	if we, ok := err.(*WorkError); ok {
		return we.Code
	}
	return EInternal
}
