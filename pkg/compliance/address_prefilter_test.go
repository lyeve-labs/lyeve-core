package compliance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// The free-text prefilter is a LIKE for each of AddressTextForms, and
// NamesAddress confirms what it selects. A payload that names the subject but
// that no form selects is the subject's data left out of the export and left
// behind by the erasure. Each payload below spells the address the way a
// sender stores it. On every dialect each one that names the subject is
// selected and confirmed, and each one that names another person is refused.
func TestAddressTextForms_PrefilterSelectsEveryPayloadThatNamesTheAddress(t *testing.T) {
	cases := []struct {
		address       string
		named, others []string
	}{
		{
			address: "jos\u00e9@x.example",
			named: []string{
				mustJSON(t, map[string]any{"to": "jos\u00e9@x.example"}),
				mustJSON(t, map[string]any{"body": mustJSON(t, map[string]any{"to": "Jos\u00e9 <jos\u00e9@x.example>"})}),
				asciiJSON(t, map[string]any{"to": "jos\u00e9@x.example"}),
				asciiJSON(t, map[string]any{"body": asciiJSON(t, map[string]any{"to": "jos\u00e9@x.example"})}),
				asciiJSON(t, map[string]any{"to": "jose\u0301@x.example"}),
				asciiJSON(t, map[string]any{"body": asciiJSON(t, map[string]any{"to": "jose\u0301@x.example"})}),
				"sent to jose\u0301@x.example",
				"Write to Jos\u00e9@X.Example.",
			},
			others: []string{
				mustJSON(t, map[string]any{"to": "jose@x.example"}),
				asciiJSON(t, map[string]any{"to": "xjos\u00e9@x.example"}),
			},
		},
		{
			address: "stra\u00dfe@x.example",
			named: []string{
				mustJSON(t, map[string]any{"to": "stra\u00dfe@x.example"}),
				"sent to STRA\u1e9eE@X.EXAMPLE",
				asciiJSON(t, map[string]any{"to": "stra\u00dfe@x.example"}),
				asciiJSON(t, map[string]any{"body": asciiJSON(t, map[string]any{"to": "stra\u00dfe@x.example"})}),
			},
			others: []string{"sent to strasse@x.example"},
		},
		{
			address: "STRA\u1e9eE@X.EXAMPLE",
			named:   []string{"sent to stra\u00dfe@x.example", "sent to Stra\u1e9ee@x.example"},
		},
		{
			address: "a&b@x.example",
			named: []string{
				mustJSON(t, map[string]any{"to": "a&b@x.example"}),
				mustJSON(t, map[string]any{"body": mustJSON(t, map[string]any{"to": "a&b@x.example"})}),
				asciiJSON(t, map[string]any{"to": "A&B@X.Example"}),
			},
			others: []string{"sent to aa&b@x.example"},
		},
		{
			address: "bob@x.example",
			named: []string{
				"https://crm.example/?to=bob%40x.example",
				mustJSON(t, map[string]any{"url": "https://crm.example/?to=Bob%40X.Example"}),
				`<a href="mailto:bob&#64;x.example">Bob</a>`,
			},
			others: []string{
				"write to alice%2Bbob@x.example",
				"https://crm.example/?to=alice%2Bbob%40x.example",
				"write to alice&#43;bob&#64;x.example",
			},
		},
	}

	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		table := "address_probe_" + uuid.NewString()[:8]
		body := "TEXT"
		if host.Dialect() == "mssql" {
			body = "NVARCHAR(MAX)"
		}
		_, err := q.Exec(ctx, "CREATE TABLE "+table+" (id INT PRIMARY KEY, grp INT NOT NULL, body "+body+" NOT NULL)")
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := q.Exec(context.Background(), "DROP TABLE "+table)
			assert.NoError(t, err)
		})

		id := 0
		for grp, tc := range cases {
			want := map[int]string{}
			for _, payload := range tc.named {
				require.True(t, compliance.NamesAddress(payload, tc.address), "%s names %s", payload, tc.address)
				id++
				_, err := q.Exec(ctx, "INSERT INTO "+table+" (id, grp, body) VALUES ($1, $2, $3)", id, grp, payload)
				require.NoError(t, err)
				want[id] = payload
			}
			for _, payload := range tc.others {
				require.False(t, compliance.NamesAddress(payload, tc.address), "%s names another person than %s", payload, tc.address)
				id++
				_, err := q.Exec(ctx, "INSERT INTO "+table+" (id, grp, body) VALUES ($1, $2, $3)", id, grp, payload)
				require.NoError(t, err)
			}
			assert.Equal(t, want, confirmedPayloads(t, host, table, grp, tc.address),
				"payloads naming %s on %s", tc.address, host.Dialect())
		}
	})
}

// confirmedPayloads runs the prefilter the way a plugin does, one ILike
// condition for each of AddressTextForms, and returns the selected rows that
// NamesAddress confirms, by id.
func confirmedPayloads(t *testing.T, host core.Host, table string, grp int, address string) map[int]string {
	t.Helper()
	ctx := context.Background()
	d := host.Dialect()
	forms := compliance.AddressTextForms(address)
	conds, args := make([]string, len(forms)), []any{grp}
	for i, form := range forms {
		conds[i] = core.ILike(d, "body", "$"+strconv.Itoa(i+2)) + core.ILikeEscapeClause(d)
		args = append(args, core.ILikeValue(d, form))
	}
	rows, err := host.Querier(ctx).Query(ctx,
		"SELECT id, body FROM "+table+" WHERE grp = $1 AND ("+strings.Join(conds, " OR ")+")", args...)
	require.NoError(t, err)
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var id int
		var payload string
		require.NoError(t, rows.Scan(&id, &payload))
		if compliance.NamesAddress(payload, address) {
			got[id] = payload
		}
	}
	require.NoError(t, rows.Err())
	return got
}

// asciiJSON encodes v as an encoder that escapes every character outside
// ASCII does, which PHP's and Python's encoders do by default. It leaves the
// characters Go's encoder escapes for HTML as they are, as those encoders do.
func asciiJSON(t *testing.T, v any) string {
	t.Helper()
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(v))
	var out strings.Builder
	for _, r := range strings.TrimSuffix(buf.String(), "\n") {
		if r < 0x80 {
			out.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&out, `\u%04x`, u)
		}
	}
	return out.String()
}
