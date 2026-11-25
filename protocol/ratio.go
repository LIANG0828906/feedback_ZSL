package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

type Ratio struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

func NewRatio(num, den int64) Ratio {
	if den < 0 {
		num, den = -num, -den
	}
	r := Ratio{Num: num, Den: den}
	r.reduce()
	return r
}

func (r Ratio) Positive() bool {
	return r.Num > 0 && r.Den > 0
}

func (r Ratio) String() string {
	if r.Den == 0 {
		return fmt.Sprintf("%d/0", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

func (r Ratio) Equal(other Ratio) bool {
	return r.Num == other.Num && r.Den == other.Den
}

func (r Ratio) Compare(other Ratio) int {
	left := new(big.Int).Mul(big.NewInt(r.Num), big.NewInt(other.Den))
	right := new(big.Int).Mul(big.NewInt(other.Num), big.NewInt(r.Den))
	return left.Cmp(right)
}

func (r *Ratio) reduce() {
	if r.Den == 0 || r.Num == 0 {
		return
	}
	a, b := r.Num, r.Den
	if a < 0 {
		a = -a
	}
	for b != 0 {
		a, b = b, a%b
	}
	r.Num /= a
	r.Den /= a
	if r.Den < 0 {
		r.Num = -r.Num
		r.Den = -r.Den
	}
}

func (r Ratio) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

func (r *Ratio) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*r = Ratio{}
		return nil
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		parsed, err := ParseRatio(text)
		if err != nil {
			return err
		}
		*r = parsed
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return fmt.Errorf("ratio must be a string or number: %w", err)
	}
	parsed, err := parseDecimalRatio(number.String())
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

func ParseRatio(text string) (Ratio, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Ratio{}, fmt.Errorf("empty ratio")
	}
	if strings.Contains(text, "/") {
		parts := strings.Split(text, "/")
		if len(parts) != 2 {
			return Ratio{}, fmt.Errorf("invalid ratio %q", text)
		}
		num, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		if err != nil {
			return Ratio{}, fmt.Errorf("invalid ratio numerator: %w", err)
		}
		den, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if err != nil {
			return Ratio{}, fmt.Errorf("invalid ratio denominator: %w", err)
		}
		if den == 0 {
			return Ratio{}, fmt.Errorf("ratio denominator must not be zero")
		}
		return NewRatio(num, den), nil
	}
	return parseDecimalRatio(text)
}

func parseDecimalRatio(text string) (Ratio, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Ratio{}, fmt.Errorf("empty ratio")
	}
	rat, ok := new(big.Rat).SetString(text)
	if !ok {
		return Ratio{}, fmt.Errorf("invalid ratio %q", text)
	}
	if rat.Sign() == 0 {
		return Ratio{Num: 0, Den: 1}, nil
	}
	num := rat.Num()
	den := rat.Denom()
	if !num.IsInt64() || !den.IsInt64() {
		return Ratio{}, fmt.Errorf("ratio %q exceeds int64 precision", text)
	}
	return NewRatio(num.Int64(), den.Int64()), nil
}
