package model

import "fmt"

// ErrorCode 是工作台统一错误码。所有跨模块返回的业务错误均携带该码，
// 便于异常注入链路与上层 UI 做确定性分支处理。
type ErrorCode string

const (
	CodeMeta    ErrorCode = "E_META"    // 采样率/声道布局等元数据缺失或非法
	CodePara    ErrorCode = "E_PARA"    // 帧参数/卷积窗/渲染块等参数越界
	CodeBound   ErrorCode = "E_BOUND"   // 推导误差上界低于实测误差
	CodeState   ErrorCode = "E_STATE"   // 帧状态机非法迁移
	CodeChan    ErrorCode = "E_CHAN"    // 通道缺失/声道布局不匹配
	CodeOrder   ErrorCode = "E_ORDER"   // 反射到达点倒置
	CodeClip    ErrorCode = "E_CLIP"    // 反射能量增益溢出
	CodeFrame   ErrorCode = "E_FRAME"   // 坏帧
	CodeDiverge ErrorCode = "E_DIVERGE" // 多执行路径结果不一致
)

// AppError 携带错误码、上下文（帧号/通道标识/字段名/指标名）与可读信息。
// Frame 为 -1 表示该错误不绑定具体帧；Channel 为空表示不绑定具体声道。
type AppError struct {
	Code    ErrorCode `json:"code"`
	Field   string    `json:"field,omitempty"`
	Frame   int       `json:"frame"`
	Channel string    `json:"channel,omitempty"`
	Metric  string    `json:"metric,omitempty"`
	Reason  string    `json:"reason"`
}

func (e *AppError) Error() string {
	s := string(e.Code)
	if e.Field != "" {
		s += " field=" + e.Field
	}
	if e.Frame >= 0 {
		s += fmt.Sprintf(" frame=%d", e.Frame)
	}
	if e.Channel != "" {
		s += " channel=" + e.Channel
	}
	if e.Metric != "" {
		s += " metric=" + e.Metric
	}
	return s + ": " + e.Reason
}

func NewError(code ErrorCode, reason string) *AppError {
	return &AppError{Code: code, Frame: -1, Reason: reason}
}

func (e *AppError) WithFrame(frame int) *AppError {
	cp := *e
	cp.Frame = frame
	return &cp
}

func (e *AppError) WithField(field string) *AppError {
	cp := *e
	cp.Field = field
	return &cp
}

func (e *AppError) WithChannel(ch string) *AppError {
	cp := *e
	cp.Channel = ch
	return &cp
}

func (e *AppError) WithMetric(metric string) *AppError {
	cp := *e
	cp.Metric = metric
	return &cp
}

// AsAppError 提取业务错误，未命中返回 nil。
func AsAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	if ae, ok := err.(*AppError); ok {
		return ae
	}
	return nil
}
