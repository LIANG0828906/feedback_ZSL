package model

import "fmt"

// 业务异常码，全部以 E_ 前缀在接口与报告中稳定出现。
const (
	ESchema  = "E_SCHEMA" // 字段缺失/结构不符
	ERange   = "E_RANGE"  // 区间越界或重叠
	EPara    = "E_PARA"   // 算法参数越界
	EQuiet   = "E_QUIET"  // 静音占比过高
	EQuality = "E_QUAL"   // 标注质量不达标
	EState   = "E_STATE"  // 窗口状态机非法迁移
	EDiv     = "E_DIV"    // 三路径结果不一致
	EIO      = "E_IO"     // 本地文件/数据库读写失败
)

// AppError 携带稳定错误码、可读信息与结构化细节（事件编号、实测边界等）。
type AppError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func NewErr(code, format string, args ...any) *AppError {
	return &AppError{Code: code, Message: fmt.Sprintf(format, args...), Details: map[string]any{}}
}

func (e *AppError) With(key string, value any) *AppError {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

func (e *AppError) Error() string {
	if len(e.Details) == 0 {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Details)
}

// ErrCode 从任意 error 中提取业务码，非 AppError 返回空串。
func ErrCode(err error) string {
	if err == nil {
		return ""
	}
	if appErr, ok := err.(*AppError); ok {
		return appErr.Code
	}
	return ""
}
