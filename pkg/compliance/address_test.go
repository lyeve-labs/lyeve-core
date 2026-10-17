package compliance_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

func TestSameAddress_OnlyTheAddressItself(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stored, requested string
		want                    bool
	}{
		{"equal", "ann@x.example", "ann@x.example", true},
		{"case folded", "Ann@X.Example", "ann@x.example", true},
		{"accent is another person", "josé@x.example", "jose@x.example", false},
		{"accent dropped is another person", "jose@x.example", "josé@x.example", false},
		{"sharp s is not ss", "straße@x.example", "strasse@x.example", false},
		{"ss is not sharp s", "strasse@x.example", "straße@x.example", false},
		{"decomposed accent", "jose\u0301@x.example", "jos\u00e9@x.example", true},
		{"composed accent against decomposed", "jos\u00e9@x.example", "jose\u0301@x.example", true},
		{"capital sharp s", "straße@x.example", "STRAẞE@X.EXAMPLE", true},
		{"a with stroke", "ⱥb@x.example", "ȺB@X.EXAMPLE", true},
		{"dotted capital I", "i@x.example", "İ@X.EXAMPLE", true},
		{"dotless i is another person", "ı@x.example", "İ@X.EXAMPLE", false},
		{"trailing space", "dana@x.example ", "dana@x.example", true},
		{"leading space in the request", "dana@x.example", " dana@x.example", true},
		{"decomposed dotted capital I", "inci@x.example", "I\u0307nci@x.example", true},
		{"dotless i against decomposed dotted capital I", "ınci@x.example", "I\u0307nci@x.example", false},
		{"longer address", "joann@x.example", "ann@x.example", false},
		{"long s is another person", "\u017fam@x.example", "sam@x.example", false},
		{"s is not long s", "sam@x.example", "\u017fam@x.example", false},
		{"micro sign is not mu", "\u00b5@x.example", "\u03bc@x.example", false},
		{"tab is another address", "dana@x.example\t", "dana@x.example", false},
	} {
		assert.Equal(t, tc.want, compliance.SameAddress(tc.stored, tc.requested), tc.name)
	}
}

func TestAddressForms_RequestThenLoweredNFCAndNFD(t *testing.T) {
	t.Parallel()
	got := compliance.AddressForms(" José@X.Example ")
	assert.Equal(t, []any{" José@X.Example ", "jos\u00e9@x.example", "jose\u0301@x.example"}, got)

	got = compliance.AddressForms("İnci@X.Example")
	assert.Equal(t, []any{"İnci@X.Example", "inci@x.example", "inci@x.example"}, got)
}

func TestAddressCandidates_NumbersPlaceholdersFromFirst(t *testing.T) {
	t.Parallel()
	assert.Equal(t,
		"(LOWER(email) = LOWER($1) OR LOWER(email) = LOWER($2) OR LOWER(email) = LOWER($3) OR LOWER(LTRIM(RTRIM(email))) = LOWER($2) OR LOWER(LTRIM(RTRIM(email))) = LOWER($3))",
		compliance.AddressCandidates("email", 1))
	assert.Equal(t,
		"(LOWER(t.to_addr) = LOWER($4) OR LOWER(t.to_addr) = LOWER($5) OR LOWER(t.to_addr) = LOWER($6) OR LOWER(LTRIM(RTRIM(t.to_addr))) = LOWER($5) OR LOWER(LTRIM(RTRIM(t.to_addr))) = LOWER($6))",
		compliance.AddressCandidates("t.to_addr", 4))
}

func TestAddressTextForms_PlainThenEscapedOnceAndTwice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		address string
		want    []string
	}{
		{"ann@x.example", []string{"ann@x.example"}},
		{" ann@x.example ", []string{"ann@x.example"}},
		{"Ann@X.Example", []string{"Ann@X.Example"}},
		{"a&b@x.example", []string{"a&b@x.example", `a\u0026b@x.example`, `a\\u0026b@x.example`}},
		{`"a b"@x.example`, []string{`"a b"@x.example`, `\"a b\"@x.example`, `\\\"a b\\\"@x.example`}},
		{"jos\u00e9@x.example", []string{
			"jos\u00e9@x.example", "jose\u0301@x.example",
			`jos\u00e9@x.example`, `jos\\u00e9@x.example`, `jose\u0301@x.example`, `jose\\u0301@x.example`,
		}},
		{"Jos\u00e9@X.Example", []string{
			"Jos\u00e9@X.Example", "jose\u0301@x.example",
			`jos\u00e9@x.example`, `jos\\u00e9@x.example`, `jose\u0301@x.example`, `jose\\u0301@x.example`,
		}},
		{"jos\u00e9&b@x.example", []string{
			"jos\u00e9&b@x.example", "jose\u0301&b@x.example",
			"jos\u00e9\\u0026b@x.example", "jos\u00e9\\\\u0026b@x.example",
			"jose\u0301\\u0026b@x.example", "jose\u0301\\\\u0026b@x.example",
			`jos\u00e9&b@x.example`, `jos\\u00e9&b@x.example`, `jose\u0301&b@x.example`, `jose\\u0301&b@x.example`,
		}},
		{"stra\u00dfe@x.example", []string{
			"stra\u00dfe@x.example", "stra\u1e9ee@x.example", `stra\u00dfe@x.example`, `stra\\u00dfe@x.example`,
		}},
		{"STRA\u1e9eE@X.EXAMPLE", []string{
			"STRA\u1e9eE@X.EXAMPLE", "stra\u00dfe@x.example", "stra\u1e9ee@x.example",
			`stra\u00dfe@x.example`, `stra\\u00dfe@x.example`,
		}},
		{"\U00010330@x.example", []string{"\U00010330@x.example", `\ud800\udf30@x.example`, `\\ud800\\udf30@x.example`}},
	} {
		assert.Equal(t, tc.want, compliance.AddressTextForms(tc.address), tc.address)
	}
}

func TestNamesAddress_WholeAddressOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{"to":"ann@x.example"}`, true},
		{`{"to":"ANN@X.Example"}`, true},
		{`{"to":"Ann <ann@x.example>"}`, true},
		{`{"to":"Ann \u003cann@x.example\u003e"}`, true},
		{`{"note":"line one\nann@x.example"}`, true},
		{`{"note":"Write to ann@x.example."}`, true},
		{`{"cc":"joann@x.example","to":"ann@x.example"}`, true},
		{`{"to":"joann@x.example, ann@x.example"}`, true},
		{`["ann@x.example"]`, true},
		{`{"ann@x.example":1}`, true},
		{`not json, mail ann@x.example`, true},
		{`{"url":"https://crm.example/v1/contacts/ann@x.example"}`, true},
		{`{"body":"email=ann@x.example&name=Ann"}`, true},
		{`mailto:ann@x.example?subject=hi`, true},
		{`'ann@x.example'`, true},
		{"`ann@x.example`", true},
		{`{ann@x.example}`, true},
		{`bob@x.example|ann@x.example`, true},
		{`ann@x.example...`, true},
		{`请联系ann@x.example获取`, true},
		{`{"body":"{\"to\":\"Ann \\u003cann@x.example\\u003e\"}"}`, true},
		{`"line\\nann@x.example"`, true},
		{`{"note":"said \"hi\" to \\u003cann@x.example\\u003e"}`, true},
		{`{"body":"{\"to\":\"Ann \\u003cann@x.example\\u003e\",\"x\":"}`, true},
		{encodedTimes(t, `{"to":"Ann <ann@x.example>"}`, 4), true},
		{`mailto:Ann%20%3Cann@x.example%3E`, true},
		{`%22ann@x.example%22`, true},
		{`%20ann@x.example`, true},
		{`ติดต่อann@x.example`, true},
		{`ຕິດຕໍ່ann@x.example`, true},
		{`ទាក់ទងann@x.example`, true},
		{`ဆက်သွယ်ann@x.example`, true},
		{`{"to":"jo_ann@x.example"}`, false},
		{`{"to":"\u00e9ann@x.example"}`, false},
		{`{"to":"jo-ann@x.example"}`, false},
		{`{"to":"joann@x.example"}`, false},
		{`{"to":"ann@x.example.org"}`, false},
		{`{"to":"jo.ann@x.example"}`, false},
		{`{"to":"jo+ann@x.example"}`, false},
		{`{"to":"ann@x.example-mail.org"}`, false},
		{`{"to":"ánn@x.example"}`, false},
		{`{"to":"éann@x.example"}`, false},
		{`{"n":42}`, false},
		{``, false},
	} {
		assert.Equal(t, tc.want, compliance.NamesAddress(tc.payload, "ann@x.example"), tc.payload)
	}
}

// The text can spell the address in another Unicode form than the request:
// decomposed, or with a capital the database folds differently from Go. A
// collation LIKE caught those rows, so the confirmation compares in the form
// SameAddress uses and keeps the boundary rules.
func TestNamesAddress_AnyUnicodeForm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text, address string
		want                bool
	}{
		{"decomposed text", `{"to":"jose\u0301@x.example"}`, "jos\u00e9@x.example", true},
		{"decomposed request", `{"to":"jos\u00e9@x.example"}`, "jose\u0301@x.example", true},
		{"dotted capital I in the text", `{"to":"\u0130nci@x.example"}`, "inci@x.example", true},
		{"decomposed dotted capital I in the text", "I\u0307nci@x.example", "inci@x.example", true},
		{"capital sharp s in the text", `{"to":"STRA\u1e9eE@X.EXAMPLE"}`, "stra\u00dfe@x.example", true},
		{"decomposed text inside a JSON body", mustJSON(t, map[string]any{"body": mustJSON(t, map[string]any{"to": "Jose\u0301 <jose\u0301@x.example>"})}), "jos\u00e9@x.example", true},
		{"decomposed accent is another person", `{"to":"jose\u0301@x.example"}`, "jose@x.example", false},
		{"dotless i is another person", `{"to":"\u0131nci@x.example"}`, "\u0130nci@x.example", false},
		{"decomposed letter before the match", "e\u0301ann@x.example", "ann@x.example", false},
		{"decomposed longer address", `{"to":"jo\u0301ann@x.example"}`, "ann@x.example", false},
	} {
		assert.Equal(t, tc.want, compliance.NamesAddress(tc.text, tc.address), tc.name)
	}
}

// An encoder that escapes every character outside ASCII spells the address
// with \u escapes, and the text around it can be encoded again. The check
// reads the decoded address, in either case of hex digit, and still refuses a
// longer address or another person.
func TestNamesAddress_NonASCIIEscaped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"once", `{"to":"jos\u00e9@x.example"}`, true},
		{"twice", `{"body":"{\"to\":\"jos\\u00e9@x.example\"}"}`, true},
		{"upper-case hex", `{"body":"{\"to\":\"jos\\u00E9@x.example\"}"}`, true},
		{"decomposed", `{"body":"{\"to\":\"jose\\u0301@x.example\"}"}`, true},
		{"in a line that is not JSON", `sent to jos\u00e9@x.example`, true},
		{"longer address", `{"body":"{\"to\":\"xjos\\u00e9@x.example\"}"}`, false},
		{"another person", `{"body":"{\"to\":\"jose@x.example\"}"}`, false},
	} {
		assert.Equal(t, tc.want, compliance.NamesAddress(tc.text, "jos\u00e9@x.example"), tc.name)
	}
}

// An address holding a character JSON escapes is named however many times
// the text around it was encoded, up to the depth the check decodes, and a
// longer address encoded the same way is still refused.
func TestNamesAddress_EscapedAddress(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"a&b@x.example", `"a b"@x.example`} {
		for _, tc := range []struct {
			text string
			want bool
		}{
			{mustJSON(t, map[string]any{"to": address}), true},
			{mustJSON(t, map[string]any{"body": mustJSON(t, map[string]any{"to": address})}), true},
			{mustJSON(t, map[string]any{"body": mustJSON(t, map[string]any{"to": "x" + address})}), false},
		} {
			assert.Equal(t, tc.want, compliance.NamesAddress(tc.text, address), "%s in %s", address, tc.text)
		}
	}
}

// encodedTimes encodes text as a JSON string n times over, the way a body
// passed through several services arrives.
func encodedTimes(t *testing.T, text string, n int) string {
	t.Helper()
	for range n {
		b, err := json.Marshal(text)
		require.NoError(t, err)
		text = string(b)
	}
	return text
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
