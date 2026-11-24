package audiocal

import "fmt"

// ErrorCode 是组件对外暴露的稳定业务码。
type ErrorCode string

const (
	CodeMeta  ErrorCode = "E_META"  // 测量记录元数据缺失/非法
	CodeCurve ErrorCode = "E_CURVE" // 目标补偿曲线非法
	CodePara  ErrorCode = "E_PARA"  // 配置参数越界
	CodeFit   ErrorCode = "E_FIT"   // 拟合无法满足残差上界
	CodeWin   ErrorCode = "E_WIN"   // 时序窗口/分段约束非法或越界
	CodeEcho  ErrorCode = "E_ECHO"  // 回环回显频响超差
	CodeState ErrorCode = "E_STATE" // 分段状态机非法迁移
	CodeDiff  ErrorCode = "E_DIFF"  // 三条导出路径结果不一致
	CodeFW    ErrorCode = "E_FW"    // 模拟设备拒收固件版本
	CodeMap   ErrorCode = "E_MAP"   // 通道映射冲突
	CodeSeg   ErrorCode = "E_SEG"   // 段缺失
	CodeStore ErrorCode = "E_STORE" // 本地持久化存储故障
)

// BizError 携带稳定业务码与可定位的诊断字段。
type BizError struct {
	Code    ErrorCode
	Message string
	Field   string // 字段名 / 频点序号 / 通道标识 / 段号
	Detail  map[string]any
}

func (e *BizError) Error() string {
	s := string(e.Code) + ": " + e.Message
	if e.Field != "" {
		s += " [" + e.Field + "]"
	}
	return s
}

func newErr(code ErrorCode, msg, field string) *BizError {
	return &BizError{Code: code, Message: msg, Field: field}
}

func newErrf(code ErrorCode, field, format string, args ...any) *BizError {
	return &BizError{Code: code, Message: fmt.Sprintf(format, args...), Field: field}
}

// AsBizError 尝试把任意 error 还原为 BizError。
func AsBizError(err error) (*BizError, bool) {
	if err == nil {
		return nil, false
	}
	if be, ok := err.(*BizError); ok {
		return be, true
	}
	return nil, false
}
