package validate

import "fmt"

// 业务错误码。命名与任务要求一一对应。
const (
	EOrder   = "E_ORDER"   // 帧序非单调
	EChan    = "E_CHAN"    // 通道越界
	ETpl     = "E_TPL"     // 模板引用缺失
	EPara    = "E_PARA"    // 量化单位/每包采样点数等参数非法
	EState   = "E_STATE"   // 状态机非法迁移
	EDiff    = "E_DIFF"    // 路径间结果不一致
	EMiss    = "E_MISS"    // 设备侧缺包
	EDup     = "E_DUP"     // 设备侧重发
	ETimeout = "E_TIMEOUT" // 设备侧超时
	ELocal   = "E_LOCAL"   // 内核侧本地违规（与设备侧异常严格区分）
	EStorage = "E_STORAGE" // 持久化读写失败
)

// BizError 是带定位信息的业务错误。
type BizError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Frame   int    `json:"frame,omitempty"`
	Channel int    `json:"channel,omitempty"`
	Field   string `json:"field,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

func (e *BizError) Error() string {
	s := e.Code + ": " + e.Message
	if e.Field != "" {
		s += " (field=" + e.Field + ")"
	}
	if e.Channel >= 0 {
		s += fmt.Sprintf(" (channel=%d)", e.Channel)
	}
	if e.Frame >= 0 {
		s += fmt.Sprintf(" (frame=%d)", e.Frame)
	}
	if e.Detail != "" {
		s += " " + e.Detail
	}
	return s
}

// NewBiz 构造业务错误，通道/帧默认 -1 表示不适用。
func NewBiz(code, msg, field string, ch, frame int) *BizError {
	return &BizError{Code: code, Message: msg, Field: field, Channel: ch, Frame: frame}
}
