package errx

import (
	"errors"
	"fmt"
	"strings"
)

// 全部领域错误码集中声明，便于测试与诊断页面稳定引用。
const (
	ESchema   = "E_SCHEMA"   // schema 版本未知 / 必填字段缺失
	ELossy    = "E_LOSSY"    // 降级到低版本时无法无损表达
	EOvf      = "E_OVF"      // 整型换算溢出
	EEval     = "E_EVAL"     // 表达式求值错误（除零、负数开方等）
	EConflict = "E_CONFLICT" // 同 tick 争用无法裁决
	EState    = "E_STATE"    // 状态机非法迁移
	EStore    = "E_STORE"    // 检查点持久化错误
	EEntry    = "E_ENTRY"    // 检查点版本不匹配整窗作废
)

// Error 是领域内唯一的结构化错误类型。Fields 以字符串键保存诊断上下文，
// 例如字段路径、来源版本、表达式节点、操作数、tick、技能标识等。
type Error struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
	Cause   error             `json:"-"`
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code)
	b.WriteString(": ")
	b.WriteString(e.Message)
	for k, v := range e.Fields {
		fmt.Fprintf(&b, " %s=%q", k, v)
	}
	if e.Cause != nil {
		b.WriteString(" cause=")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Cause }

// New 构造一个结构化错误。
func New(code, format string, args ...any) *Error {
	return &Error{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
		Fields:  map[string]string{},
	}
}

// With 追加诊断字段并返回原错误，便于链式构造。
func (e *Error) With(kv ...string) *Error {
	if e.Fields == nil {
		e.Fields = map[string]string{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Fields[kv[i]] = kv[i+1]
	}
	return e
}

// Wrap 关联底层错误。
func (e *Error) Wrap(cause error) *Error {
	e.Cause = cause
	return e
}

// CodeOf 提取错误码；非领域错误返回空串。
func CodeOf(err error) string {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// Is 支持 errors.Is 按错误码比较。
func (e *Error) Is(target error) bool {
	var de *Error
	if errors.As(target, &de) {
		return de.Code == e.Code
	}
	return false
}
