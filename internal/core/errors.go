package core

import "fmt"

// 业务约定的错误码。所有跨包错误都以 ErrorCode 形式携带稳定码与定位信息。
const (
	ESchema  = "E_SCHEMA"  // 字段缺失 / 结构不合法
	EBox     = "E_BOX"     // 包围体非有限或倒置
	ERef     = "E_REF"     // 引用越界 / 引用不存在
	EPara    = "E_PARA"    // 参数越界
	EState   = "E_STATE"   // 非法状态迁移
	EBudget  = "E_BUDGET"  // 槽位 / 预算参数非法
	EDiverge = "E_DIVERGE" // 三路径结果不一致
	EIO      = "E_IO"      // 本地 IO / 持久化错误
)

// CodedError 携带稳定错误码、可读信息与可选定位字段。
type CodedError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

func (e *CodedError) Error() string {
	if len(e.Fields) == 0 {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s %v", e.Code, e.Message, e.Fields)
}

// NewError 构造一个带错误码的错误。
func NewError(code, message string, fields map[string]any) *CodedError {
	return &CodedError{Code: code, Message: message, Fields: fields}
}

// CodeOf 提取错误码，非 CodedError 统一归为 E_IO。
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	if ce, ok := err.(*CodedError); ok {
		return ce.Code
	}
	return EIO
}
