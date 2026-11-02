package core

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegerValue_TakesOnlyExactIntegers(t *testing.T) {
	ok := []struct {
		in   any
		want int64
	}{
		{int64(math.MaxInt64), math.MaxInt64},
		{int64(math.MinInt64), math.MinInt64},
		{42, 42},
		{uint64(math.MaxInt64), math.MaxInt64},
		{float64(1<<53 - 1), 1<<53 - 1},
		{-float64(1<<53 - 1), -(1<<53 - 1)},
		{-12.0, -12},
		{json.Number("9007199254740993"), 9007199254740993},
		{json.Number("-9223372036854775808"), math.MinInt64},
		{"9223372036854775807", math.MaxInt64},
	}
	for _, c := range ok {
		got, err := IntegerValue(c.in)
		require.NoError(t, err, "%#v", c.in)
		assert.Equal(t, c.want, got, "%#v", c.in)
	}

	refused := []any{
		12.5,
		// 2^53 is also the float64 nearest 2^53+1, so a client that sent
		// 9007199254740993 and decoded it as a float arrives here.
		float64(1 << 53),
		float64(9007199254740993),
		-float64(1 << 53),
		float64(1<<53) + 2,
		math.NaN(),
		math.Inf(1),
		uint64(math.MaxInt64) + 1,
		json.Number("9223372036854775808"),
		json.Number("1e3"),
		json.Number("12.0"),
		"",
		" 12",
		true,
		nil,
	}
	for _, in := range refused {
		_, err := IntegerValue(in)
		assert.ErrorIs(t, err, ErrNumericForm, "%#v", in)
	}
}

func TestDecimalValue_AnswersTheCanonicalText(t *testing.T) {
	cases := []struct {
		in       any
		p, s     int
		want     string
		wantErrs error
	}{
		{"19.9", 10, 2, "19.90", nil},
		{json.Number("19.90"), 10, 2, "19.90", nil},
		{19.9, 10, 2, "19.90", nil},
		{"0.5", 10, 2, "0.50", nil},
		{".5", 10, 2, "0.50", nil},
		{"-0.50", 10, 2, "-0.50", nil},
		{"-0.00", 10, 2, "0.00", nil},
		{"+7", 10, 2, "7.00", nil},
		{"007.10", 10, 2, "7.10", nil},
		{"19.900", 10, 2, "19.90", nil},
		{int64(12), 10, 0, "12", nil},
		{"12.", 10, 0, "12", nil},
		{"99999999.99", 10, 2, "99999999.99", nil},
		{"9999999999999999999999999999.9999999999", 38, 10, "9999999999999999999999999999.9999999999", nil},
		{"-9999999999999999999999999999.9999999999", 38, 10, "-9999999999999999999999999999.9999999999", nil},
		{"1.005", 10, 2, "", ErrDecimalPrecision},
		{1.005, 10, 2, "", ErrDecimalPrecision},
		{1234567890123.45, 38, 2, "1234567890123.45", nil},
		// A float64 with more than 15 significant digits may not be the
		// number its writer meant, so it is refused rather than printed.
		{12345678901234567.0, 38, 2, "", ErrNumericForm},
		{0.1234567890123456, 38, 20, "", ErrNumericForm},
		{"100000000.00", 10, 2, "", ErrDecimalPrecision},
		{"1e3", 10, 2, "", ErrNumericForm},
		{"NaN", 10, 2, "", ErrNumericForm},
		{math.Inf(-1), 10, 2, "", ErrNumericForm},
		{"", 10, 2, "", ErrNumericForm},
		{".", 10, 2, "", ErrNumericForm},
		{"1.2.3", 10, 2, "", ErrNumericForm},
		{"--1", 10, 2, "", ErrNumericForm},
		{true, 10, 2, "", ErrNumericForm},
		{"1", 0, 0, "", ErrDecimalSpec},
		{"1", 39, 2, "", ErrDecimalSpec},
		{"1", 38, 31, "", ErrDecimalSpec},
		{"1", 2, 3, "", ErrDecimalSpec},
	}
	for _, c := range cases {
		got, err := DecimalValue(c.in, c.p, c.s)
		if c.wantErrs != nil {
			assert.True(t, errors.Is(err, c.wantErrs), "%#v at (%d,%d): got %q, %v", c.in, c.p, c.s, got, err)
			continue
		}
		require.NoError(t, err, "%#v", c.in)
		assert.Equal(t, c.want, got, "%#v", c.in)
	}
}

func TestDecimalSpec_DefaultsAndDeclaredValues(t *testing.T) {
	p, s := DecimalSpec(SchemaField{FieldType: "decimal"})
	assert.Equal(t, []int{18, 2}, []int{p, s})

	zero, one, ten := 0, 1, 10
	p, s = DecimalSpec(SchemaField{FieldType: "decimal", Precision: &ten, Scale: &zero})
	assert.Equal(t, []int{10, 0}, []int{p, s}, "a declared scale of 0 is kept")

	p, s = DecimalSpec(SchemaField{FieldType: "decimal", Precision: &one})
	assert.Equal(t, []int{1, 1}, []int{p, s}, "the default scale never passes the precision")
}

func TestCompareDecimal_OrdersByValue(t *testing.T) {
	ordered := []string{"-100.5", "-19.90", "-19.9", "-0.5", "0", "0.00", "0.05", "0.5", "19.9", "19.90", "19.91", "100"}
	for i := range ordered {
		for j := range ordered {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			// Equal values sit side by side above.
			if eq := map[[2]string]bool{
				{"-19.90", "-19.9"}: true, {"-19.9", "-19.90"}: true,
				{"0", "0.00"}: true, {"0.00", "0"}: true,
				{"19.9", "19.90"}: true, {"19.90", "19.9"}: true,
			}; eq[[2]string{ordered[i], ordered[j]}] {
				want = 0
			}
			assert.Equal(t, want, CompareDecimal(ordered[i], ordered[j]), "%s vs %s", ordered[i], ordered[j])
		}
	}
	assert.Equal(t, 0, CompareDecimal("-0.00", "0"))
}
