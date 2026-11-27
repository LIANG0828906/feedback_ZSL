// Package fixed 实现全链路统一的整型定点换算。
//
// 内部统一口径：
//   - 时间单位为逻辑 tick（TickMillis 毫秒/tick）；
//   - 比例刻度为内部万分比（InternalScale=10000）；
//   - 全部运算使用 int64，禁止浮点，余数按声明的取舍方向处理并记录残差。
package fixed

import (
	"math"
	"strconv"

	"skillsched/internal/errx"
)

const (
	// Permille 是千分比刻度（schema v1/v2）。
	Permille int64 = 1000
	// Permyriad 是万分比刻度（内部口径与 schema v3）。
	Permyriad int64 = 10000
	// InternalScale 是内部统一比例刻度。
	InternalScale = Permyriad
	// TickMillis 是一个逻辑 tick 对应的毫秒数（声明常量）。
	TickMillis int64 = 100
)

// Round 声明取舍方向。
type Round string

const (
	RoundDown Round = "down" // 向零截断（默认，排程口径保守）
	RoundUp   Round = "up"   // 远离零方向进位
	RoundNear Round = "near" // 四舍五入，.5 向上
)

// Truncation 记录一次发生余数处理的换算位置。
type Truncation struct {
	Where     string `json:"where"`     // 截断位置（字段路径/调用点）
	Input     int64  `json:"input"`     // 输入值
	Divisor   int64  `json:"divisor"`   // 除数
	Quotient  int64  `json:"quotient"`  // 取舍后的商
	Remainder int64  `json:"remainder"` // 被处理掉的余数（残差，恒非负）
	Round     Round  `json:"round"`     // 取舍方向
}

// Trace 累积一次装载/导出过程中的全部截断记录。
type Trace struct {
	Items []Truncation `json:"items"`
}

func (t *Trace) add(x Truncation) {
	if x.Remainder != 0 {
		t.Items = append(t.Items, x)
	}
}

// mul64 / add64 在溢出时返回 false，避免静默回绕。
func mul64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	r := a * b
	if r/b != a {
		return 0, false
	}
	return r, true
}

func add64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func ovf(field string, a, b int64, op string) *errx.Error {
	return errx.New(errx.EOvf, "integer overflow in %s", field).
		With("field", field, "op", op, "operand_a", strconv.FormatInt(a, 10), "operand_b", strconv.FormatInt(b, 10))
}

func applyDiv(a, b int64, r Round) (q, rem int64) {
	// b 恒为正（刻度或 tick 毫秒）。
	q = a / b
	rem = a - q*b // 与 a 同号区间 [0,b)
	absRem := rem
	if absRem < 0 {
		absRem = -rem
	}
	switch r {
	case RoundUp:
		if absRem != 0 {
			if a >= 0 {
				q++
			} else {
				q--
			}
		}
	case RoundNear:
		if absRem*2 >= b {
			if a >= 0 {
				q++
			} else {
				q--
			}
		}
	default: // RoundDown 向零截断，商不变
	}
	return q, absRem
}

// Scale 将 value*fromScale 换算到 toScale，纯整型，记录截断与残差。
// where 用于在转换痕迹中定位字段。
func Scale(tr *Trace, where string, value, fromScale, toScale int64, r Round) (int64, error) {
	if fromScale <= 0 || toScale <= 0 {
		return 0, errx.New(errx.EOvf, "scale must be positive").
			With("field", where, "from_scale", strconv.FormatInt(fromScale, 10), "to_scale", strconv.FormatInt(toScale, 10))
	}
	step, ok := mul64(value, toScale)
	if !ok {
		return 0, ovf(where, value, toScale, "mul")
	}
	out, rem := applyDiv(step, fromScale, r)
	if tr != nil {
		tr.add(Truncation{Where: where, Input: value, Divisor: fromScale, Quotient: out, Remainder: rem, Round: r})
	}
	return out, nil
}

// ToInternal 把任意刻度值换算为内部万分比。
func ToInternal(tr *Trace, where string, value, fromScale int64, r Round) (int64, error) {
	return Scale(tr, where, value, fromScale, InternalScale, r)
}

// FromInternal 把内部万分比换算到目标刻度。
func FromInternal(tr *Trace, where string, value, toScale int64, r Round) (int64, error) {
	return Scale(tr, where, value, InternalScale, toScale, r)
}

// MsToTicks 毫秒转内部 tick。
func MsToTicks(tr *Trace, where string, ms int64, r Round) (int64, error) {
	out, rem := applyDiv(ms, TickMillis, r)
	if tr != nil {
		tr.add(Truncation{Where: where, Input: ms, Divisor: TickMillis, Quotient: out, Remainder: rem, Round: r})
	}
	return out, nil
}

// TicksToMs 内部 tick 转毫秒，溢出返回 E_OVF（含两个操作数）。
func TicksToMs(tr *Trace, where string, ticks int64) (int64, error) {
	out, ok := mul64(ticks, TickMillis)
	if !ok {
		return 0, ovf(where, ticks, TickMillis, "mul")
	}
	return out, nil
}

// Add 带溢出保护的加法。
func Add(field string, a, b int64) (int64, error) {
	out, ok := add64(a, b)
	if !ok {
		return 0, ovf(field, a, b, "add")
	}
	return out, nil
}

// Mul 带溢出保护的乘法。
func Mul(field string, a, b int64) (int64, error) {
	out, ok := mul64(a, b)
	if !ok {
		return 0, ovf(field, a, b, "mul")
	}
	return out, nil
}

// Sqrt 纯整型非负开方（牛顿迭代，向下取整）。负数返回 E_EVAL。
func Sqrt(where string, x int64) (int64, error) {
	if x < 0 {
		return 0, errx.New(errx.EEval, "negative sqrt operand").
			With("node", where, "operand_a", strconv.FormatInt(x, 10), "operand_b", "")
	}
	if x == 0 {
		return 0, nil
	}
	// r = ceil(sqrt(x)) 的牛顿迭代，初值取 x。
	r := x
	for {
		nr := (r + x/r) / 2
		if nr >= r {
			break
		}
		r = nr
	}
	return r, nil
}
