package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The bounds of a decimal field. 38 is the widest precision SQL Server
// stores, and 30 the largest scale MySQL stores, so a value inside both moves
// between the three databases unchanged.
const (
	DecimalMinPrecision     = 1
	DecimalMaxPrecision     = 38
	DecimalMaxScale         = 30
	DecimalDefaultPrecision = 18
	DecimalDefaultScale     = 2
)

// maxExactFloat is 2^53. Below it every whole float64 is the only integer
// that reads as it. 2^53 itself is also what 2^53+1 rounds to, so a float64
// at or past it may be a neighbor of the number its writer meant.
const maxExactFloat = 1 << 53

var (
	// ErrNumericForm is a value that is not a number of the field's kind:
	// not a whole number for an integer field, not a plain decimal for a
	// decimal field, or outside the int64 range.
	ErrNumericForm = errors.New("not an exact number of the field's type")

	// ErrDecimalPrecision is a decimal with more integer digits than the
	// field's precision leaves room for, or more fractional digits than its
	// scale. It is refused rather than rounded, because each database rounds
	// by its own rule.
	ErrDecimalPrecision = errors.New("more digits than the field's precision and scale hold")

	// ErrDecimalSpec is a precision or scale outside the bounds above.
	ErrDecimalSpec = errors.New("decimal precision or scale out of range")
)

// DecimalSpec answers the precision and scale of a decimal field, 18 and 2
// for a field that declares neither. A default scale never exceeds the
// declared precision.
func DecimalSpec(f SchemaField) (precision, scale int) {
	precision, scale = DecimalDefaultPrecision, DecimalDefaultScale
	if f.Precision != nil {
		precision = *f.Precision
	}
	if f.Scale != nil {
		scale = *f.Scale
	} else if scale > precision {
		scale = precision
	}
	return precision, scale
}

// IntegerValue answers v as an int64 when it is one exactly: an integer kind
// within the int64 range, a whole float64 smaller than 2^53 in magnitude,
// or a json.Number or string strconv.ParseInt reads. Anything else is
// ErrNumericForm.
func IntegerValue(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int8:
		return int64(n), nil
	case uint64:
		if n > math.MaxInt64 {
			return 0, fmt.Errorf("%w: %d", ErrNumericForm, n)
		}
		return int64(n), nil
	case uint:
		if uint64(n) > math.MaxInt64 {
			return 0, fmt.Errorf("%w: %d", ErrNumericForm, n)
		}
		return int64(n), nil
	case uint32:
		return int64(n), nil
	case uint16:
		return int64(n), nil
	case uint8:
		return int64(n), nil
	case float64:
		return wholeFloat(n)
	case float32:
		return wholeFloat(float64(n))
	case json.Number:
		return parseInteger(string(n))
	case string:
		return parseInteger(n)
	}
	return 0, fmt.Errorf("%w: %T", ErrNumericForm, v)
}

func wholeFloat(f float64) (int64, error) {
	if f != math.Trunc(f) || math.Abs(f) >= maxExactFloat {
		return 0, fmt.Errorf("%w: %v", ErrNumericForm, f)
	}
	return int64(f), nil
}

func parseInteger(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrNumericForm, s)
	}
	return n, nil
}

// DecimalValue answers v as the canonical text of a decimal(precision,
// scale): an optional minus sign, the integer digits with no leading zero
// (a lone 0 when there are none), and exactly scale fractional digits. A
// string or json.Number is read as a plain decimal (a sign, digits, an
// optional point and digits). Exponent forms, NaN, infinities and the empty
// string are ErrNumericForm. A float64 is read as the shortest text that
// round-trips it, so 19.9 is 19.90 at scale 2. A float64 whose shortest text
// has more than 15 significant digits is ErrNumericForm, because a float64
// holds only 15 decimal digits for certain and the rest may be its rounding
// of what its writer sent. Trailing fractional zeros
// beyond the scale change no value and are dropped. Any other digit past what
// the field holds is ErrDecimalPrecision, never rounded.
func DecimalValue(v any, precision, scale int) (string, error) {
	if precision < DecimalMinPrecision || precision > DecimalMaxPrecision ||
		scale < 0 || scale > DecimalMaxScale || scale > precision {
		return "", fmt.Errorf("%w: (%d,%d)", ErrDecimalSpec, precision, scale)
	}
	var text string
	switch n := v.(type) {
	case string:
		text = n
	case json.Number:
		text = string(n)
	case float64:
		if !exactFloat(n, 64) {
			return "", fmt.Errorf("%w: %v", ErrNumericForm, n)
		}
		text = strconv.FormatFloat(n, 'f', -1, 64)
	case float32:
		if !exactFloat(float64(n), 32) {
			return "", fmt.Errorf("%w: %v", ErrNumericForm, n)
		}
		text = strconv.FormatFloat(float64(n), 'f', -1, 32)
	default:
		i, err := IntegerValue(v)
		if err != nil {
			return "", fmt.Errorf("%w: %T", ErrNumericForm, v)
		}
		text = strconv.FormatInt(i, 10)
	}
	neg, intDigits, frac, ok := splitDecimal(text)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNumericForm, text)
	}
	if len(intDigits) > precision-scale || len(frac) > scale {
		return "", fmt.Errorf("%w: %q in (%d,%d)", ErrDecimalPrecision, text, precision, scale)
	}
	out := make([]byte, 0, len(intDigits)+scale+3)
	if neg && (intDigits != "" || frac != "") {
		out = append(out, '-')
	}
	if intDigits == "" {
		out = append(out, '0')
	} else {
		out = append(out, intDigits...)
	}
	if scale > 0 {
		out = append(out, '.')
		out = append(out, frac...)
		for range scale - len(frac) {
			out = append(out, '0')
		}
	}
	return string(out), nil
}

// maxFloatDigits is the number of significant decimal digits every float64
// holds exactly.
const maxFloatDigits = 15

// exactFloat reports whether f is finite and its shortest text at the given
// bit size has no more than maxFloatDigits significant digits.
func exactFloat(f float64, bitSize int) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	mantissa, _, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, bitSize), "e")
	digits := 0
	for i := 0; i < len(mantissa); i++ {
		if isDigit(mantissa[i]) {
			digits++
		}
	}
	return digits <= maxFloatDigits
}

// splitDecimal reads a plain decimal into its sign, its integer digits with
// leading zeros removed and its fractional digits with trailing zeros
// removed. It reports false for anything else.
func splitDecimal(s string) (neg bool, intDigits, frac string, ok bool) {
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	start := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	intDigits = s[start:i]
	if i < len(s) && s[i] == '.' {
		i++
		fs := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		frac = s[fs:i]
	}
	if i != len(s) || len(intDigits)+len(frac) == 0 {
		return false, "", "", false
	}
	return neg, strings.TrimLeft(intDigits, "0"), strings.TrimRight(frac, "0"), true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// CompareDecimal orders two plain decimals, such as the canonical texts
// DecimalValue answers, by value: -1 when a is less than b, 0 when equal and
// 1 when greater. A text that is not a plain decimal orders as zero.
func CompareDecimal(a, b string) int {
	an, ai, af, _ := splitDecimal(a)
	bn, bi, bf, _ := splitDecimal(b)
	aZero, bZero := ai == "" && af == "", bi == "" && bf == ""
	an, bn = an && !aZero, bn && !bZero
	if an != bn {
		if an {
			return -1
		}
		return 1
	}
	c := compareMagnitude(ai, af, bi, bf)
	if an {
		return -c
	}
	return c
}

// compareMagnitude orders two unsigned decimals given as integer digits with
// no leading zero and fractional digits with no trailing zero.
func compareMagnitude(ai, af, bi, bf string) int {
	if len(ai) != len(bi) {
		if len(ai) < len(bi) {
			return -1
		}
		return 1
	}
	if c := strings.Compare(ai, bi); c != 0 {
		return c
	}
	return strings.Compare(af, bf)
}
