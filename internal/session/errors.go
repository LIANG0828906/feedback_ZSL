package session

import "fmt"

// 业务错误码。所有跨层返回的结构化错误均使用这些码。
const (
	EMeta    = "E_META"    // 清单字段缺失/点数不符/采样率不一致/通道标识冲突
	EDecode  = "E_DECODE"  // 裸采样数据不可解码（长度、编码不合法）
	EDrift   = "E_DRIFT"   // 标记不足或漂移估计退化
	EPara    = "E_PARA"    // 漂移率或插值参数越界
	EResid   = "E_RESID"   // 对齐后残余偏移超出声明上界
	EState   = "E_STATE"   // 分段状态机非法迁移
	EDiverge = "E_DIVERGE" // 多路径结果逐样本比对不一致
	EChan    = "E_CHAN"    // 注入/检测到通道缺失
	EMark    = "E_MARK"    // 注入/检测到对齐标记缺失
	EClip    = "E_CLIP"    // 注入/检测到增益溢出（削顶）
)

// Error 携带错误码与定位信息（通道、字段、分段、标记、采样位置）。
type Error struct {
	Code    string
	Message string
	Channel string
	Field   string
	Segment int
	Marker  int
	Sample  int64
	Cause   error
}

func (e *Error) Error() string {
	s := e.Code + ": " + e.Message
	if e.Channel != "" {
		s += " [channel=" + e.Channel + "]"
	}
	if e.Field != "" {
		s += " [field=" + e.Field + "]"
	}
	if e.Segment >= 0 {
		s += fmt.Sprintf(" [segment=%d]", e.Segment)
	}
	if e.Marker >= 0 {
		s += fmt.Sprintf(" [marker=%d]", e.Marker)
	}
	if e.Sample >= 0 {
		s += fmt.Sprintf(" [sample=%d]", e.Sample)
	}
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError 构造一个结构化错误。
func NewError(code, msg string) *Error {
	return &Error{Code: code, Message: msg, Segment: -1, Marker: -1, Sample: -1}
}

// With 链式补充通道与字段定位。
func (e *Error) With(channel, field string) *Error {
	e.Channel = channel
	e.Field = field
	return e
}

func (e *Error) WithSegment(seg int) *Error  { e.Segment = seg; return e }
func (e *Error) WithMarker(m int) *Error     { e.Marker = m; return e }
func (e *Error) WithSample(idx int64) *Error { e.Sample = idx; return e }

// CodeOf 提取错误码，非结构化错误返回空串。
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	if se, ok := err.(*Error); ok {
		return se.Code
	}
	return ""
}
