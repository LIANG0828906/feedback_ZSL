package model

import "fmt"

// 类型化错误码：四类异常注入与运行期校验共用。
const (
	EDecode   = "E_DECODE"   // 回放损坏 / JSON 解析失败
	ESchema   = "E_SCHEMA"   // 字段缺失或不合法
	EOrder    = "E_ORDER"    // 回合时序倒置 / 跳号
	EConflict = "E_CONFLICT" // 无违规却裁决失败等状态冲突
	EStage    = "E_STAGE"    // 单窗口结算内部失败
	EState    = "E_STATE"    // 窗口状态机非法迁移 / 重叠窗口
	EDiff     = "E_DIFF"     // 三路径或复核出现不一致
)

// KernelError 携带错误码与可定位的上下文字段。
type KernelError struct {
	Code     string
	Message  string
	MatchID  string
	WindowNo int
	TurnNo   int
	Field    string
	Expected int64
	Got      int64
	Ruling   *Ruling
}

func (e *KernelError) Error() string {
	s := e.Code + ": " + e.Message
	if e.Field != "" {
		s += " (field=" + e.Field + ")"
	}
	if e.TurnNo != 0 || e.WindowNo != 0 || e.MatchID != "" {
		s += fmt.Sprintf(" [match=%s window=%d turn=%d]", e.MatchID, e.WindowNo, e.TurnNo)
	}
	return s
}

// NewError 构造基础错误。
func NewError(code, format string, args ...any) *KernelError {
	return &KernelError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithMatch 附加对局定位。
func (e *KernelError) WithMatch(matchID string) *KernelError {
	e.MatchID = matchID
	return e
}

// WithWindow 附加窗口定位。
func (e *KernelError) WithWindow(no int) *KernelError {
	e.WindowNo = no
	return e
}

// WithTurn 附加回合定位。
func (e *KernelError) WithTurn(no int) *KernelError {
	e.TurnNo = no
	return e
}

// WithField 附加字段路径与期望/实测值。
func (e *KernelError) WithField(field string, expected, got int64) *KernelError {
	e.Field = field
	e.Expected = expected
	e.Got = got
	return e
}

// AsKernelError 从 error 提取内核错误。
func AsKernelError(err error) (*KernelError, bool) {
	if err == nil {
		return nil, false
	}
	if as, ok := err.(*KernelError); ok {
		return as, true
	}
	return nil, false
}
