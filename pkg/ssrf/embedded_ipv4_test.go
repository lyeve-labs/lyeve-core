package ssrf

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The shared address space and the IPv4 address a NAT64 or 6to4 address
// embeds are judged like any other destination: refused as a literal, as a
// name that resolves to it, and again when the transport dials.
func TestReservedAndEmbeddedRanges(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		blocked bool
	}{
		{"alibaba cloud metadata", "100.100.100.200", true},
		{"bottom of shared space", "100.64.0.1", true},
		{"top of shared space", "100.127.255.254", true},
		{"just above shared space", "100.128.0.1", false},
		{"nat64 of link-local metadata", "64:ff9b::a9fe:a9fe", true},
		{"nat64 of loopback", "64:ff9b::7f00:1", true},
		{"nat64 of shared space", "64:ff9b::6464:64c8", true},
		{"nat64 of a public address", "64:ff9b::5db8:d822", false},
		{"local-use nat64", "64:ff9b:1::a9fe:a9fe", true},
		{"local-use nat64 of a public address", "64:ff9b:1::5db8:d822", true},
		{"6to4 of link-local metadata", "2002:a9fe:a9fe::1", true},
		{"6to4 of private", "2002:0a00:0001::", true},
		{"6to4 of shared space", "2002:6464:64c8::1", true},
		{"6to4 of a public address", "2002:5db8:d822::1", false},
		{"ipv4-compatible of link-local metadata", "::a9fe:a9fe", true},
		{"ipv4-compatible of a public address", "::5db8:d822", true},
		{"siit of link-local metadata", "::ffff:0:a9fe:a9fe", true},
		{"siit of a public address", "::ffff:0:5db8:d822", true},
		{"teredo stays admitted", "2001:0:4136:e378:8000:63bf:3fff:fdd2", false},
	}

	names := map[string][]net.IPAddr{}
	for i, c := range cases {
		names[hostFor(i)] = []net.IPAddr{ipAddr(c.addr)}
	}
	defer withResolver(mockResolver(names))()
	ctx := context.Background()

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			literal := "http://" + net.JoinHostPort(c.addr, "80") + "/"
			resolved := "http://" + hostFor(i) + "/"
			for _, target := range []string{literal, resolved} {
				err := ValidateURL(ctx, target)
				if c.blocked {
					assert.ErrorContains(t, err, "ssrf", target)
				} else {
					assert.NoError(t, err, target)
				}
			}
			addrErr := ValidateAddress(ctx, net.JoinHostPort(c.addr, "443"))
			assert.Equal(t, c.blocked, addrErr != nil, "ValidateAddress %s: %v", c.addr, addrErr)

			if c.blocked {
				_, dialErr := NewSafeTransport().DialContext(ctx, "tcp", net.JoinHostPort(hostFor(i), "80"))
				assert.ErrorContains(t, dialErr, "ssrf dial", "the dial refuses a name resolving to %s", c.addr)
			}
		})
	}
}

func hostFor(i int) string { return "target" + string(rune('a'+i)) + ".example.test" }

// An install that routes the shared address space on purpose allows it, and
// the allowance covers the same address in its NAT64 form. It opens nothing
// else.
func TestAllowlist_SharedAddressSpace(t *testing.T) {
	t.Cleanup(ClearAllowlist)
	ClearAllowlist()
	require.NoError(t, AddAllowlist("100.64.0.0/10"))
	ctx := context.Background()

	assert.NoError(t, ValidateURL(ctx, "http://100.101.102.103/"))
	assert.NoError(t, ValidateURL(ctx, "http://[64:ff9b::6465:6667]/"))
	assert.Error(t, ValidateURL(ctx, "http://[64:ff9b::a9fe:a9fe]/"))
	assert.Error(t, ValidateURL(ctx, "http://169.254.169.254/"))
}

// ctxHandler keeps the context each record was logged with.
type ctxHandler struct{ got *[]context.Context }

func (h ctxHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h ctxHandler) Handle(ctx context.Context, _ slog.Record) error {
	*h.got = append(*h.got, ctx)
	return nil
}
func (h ctxHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h ctxHandler) WithGroup(string) slog.Handler      { return h }

type tenantKey struct{}

// The refusal is logged with the caller's context, so a handler that reads
// the tenant or the trace from it can put them on the line.
func TestRefusal_LogsWithCallerContext(t *testing.T) {
	var got []context.Context
	orig := slog.Default()
	slog.SetDefault(slog.New(ctxHandler{got: &got}))
	t.Cleanup(func() { slog.SetDefault(orig) })

	ctx := context.WithValue(context.Background(), tenantKey{}, "acme")
	require.Error(t, ValidateURL(ctx, "http://100.100.100.200/"))
	require.Len(t, got, 1)
	assert.Equal(t, "acme", got[0].Value(tenantKey{}))
}

// The log names the class of the refused range. The error, which a caller
// may hand to a client, does not.
func TestRefusal_LogsRangeClassOnly(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	ctx := context.Background()

	for _, tc := range []struct{ target, class string }{
		{"http://100.100.100.200/", "shared address space"},
		{"http://[64:ff9b::a9fe:a9fe]/", "link-local in a NAT64 address"},
		{"http://[2002:0a00:0001::]/", "private in a 6to4 address"},
	} {
		buf.Reset()
		err := ValidateURL(ctx, tc.target)
		require.Error(t, err)
		assert.Contains(t, buf.String(), `range_class="`+tc.class+`"`, tc.target)
		assert.NotContains(t, err.Error(), tc.class, tc.target)
	}
}
