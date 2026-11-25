package scene

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSecondsToMicros converts a decimal seconds token to integer
// microseconds without going through float64.
func ParseSecondsToMicros(token string) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, fmt.Errorf("empty seconds value")
	}
	if strings.HasPrefix(token, "+") {
		token = token[1:]
	}
	negative := false
	if strings.HasPrefix(token, "-") {
		negative = true
		token = token[1:]
	}
	if token == "" {
		return 0, fmt.Errorf("invalid seconds value")
	}
	parts := strings.Split(token, ".")
	if len(parts) > 2 {
		return 0, fmt.Errorf("invalid seconds value %q", token)
	}
	wholePart := parts[0]
	if wholePart == "" {
		wholePart = "0"
	}
	whole, err := strconv.ParseInt(wholePart, 10, 64)
	if err != nil || whole < 0 {
		return 0, fmt.Errorf("invalid seconds value %q", token)
	}
	frac := ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if len(frac) > 6 {
		// Decimal microseconds are the internal resolution. Reject values that
		// cannot be represented exactly rather than silently rounding.
		if strings.Trim(frac[6:], "0") != "" {
			return 0, fmt.Errorf("seconds value %q is finer than one microsecond", token)
		}
		frac = frac[:6]
	}
	frac += strings.Repeat("0", 6-len(frac))
	fracValue, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid seconds value %q", token)
	}
	value := whole*1_000_000 + fracValue
	if negative {
		value = -value
	}
	return value, nil
}

// FormatMicrosAsSeconds renders an exact decimal seconds token using integer
// arithmetic only.
func FormatMicrosAsSeconds(us int64) string {
	sign := ""
	if us < 0 {
		sign = "-"
		us = -us
	}
	whole := us / 1_000_000
	frac := us % 1_000_000
	if frac == 0 {
		return fmt.Sprintf("%s%d", sign, whole)
	}
	text := fmt.Sprintf("%s%d.%06d", sign, whole, frac)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	return text
}
