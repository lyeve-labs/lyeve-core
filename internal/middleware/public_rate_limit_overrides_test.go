package middleware

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The public caps ship as one table sized for a single origin serving browsers
// directly. A CDN collapsing a region onto a few egress addresses, an API whose
// clients are servers, and a load test from one machine all count many callers
// as one, so the table has to be movable.
//
// The shape that matters is the merge. An operator raising one endpoint must
// not find they have dropped every other limit in the table.

// hookRoute is a public route another owner serves and meters by its own
// declaration. PUBLIC_RATE_LIMITS moves it as it moves an engine row, and
// the parser keys every entry the same way whoever owns the route.
const hookRoute = "POST:/api/v1/flows/hooks/{flow_id}"

// tokenRoute is an engine row, the content API's password grant.
const tokenRoute = "POST:/api/v1/auth/token"

func TestPublicRateLimits_NoSettingLeavesTheShippedDefaults(t *testing.T) {
	overrides, global, err := ParsePublicRateLimits(nil, "")
	require.NoError(t, err)
	assert.Empty(t, overrides, "no setting means no override")
	assert.Nil(t, global)

	// The table an unconfigured deployment gets, spelled out: a deployment
	// that sets nothing serves exactly these numbers.
	configs, shipped := DefaultPublicRateLimits()
	assert.Equal(t, PublicRateLimitConfig{Rate: 5, Burst: 10}, configs["POST:/api/admin/auth/login"])
	assert.Equal(t, PublicRateLimitConfig{Rate: 5, Burst: 10}, configs[tokenRoute])
	assert.Equal(t, PublicRateLimitConfig{Rate: 0.2, Burst: 5}, configs["POST:/api/admin/auth/device"])
	assert.Equal(t, PublicRateLimitConfig{Rate: 10, Burst: 20}, configs["POST:/api/admin/gdpr/export"])
	assert.Equal(t, &PublicRateLimitConfig{Rate: 50, Burst: 100}, shipped)

	// Merging nothing over the defaults is still the defaults.
	assert.Equal(t, configs, MergePublicRateLimits(configs, overrides))
}

func TestPublicRateLimits_AnOverrideMergesOverTheTable(t *testing.T) {
	overrides, global, err := ParsePublicRateLimits([]string{tokenRoute + "=20:100"}, "")
	require.NoError(t, err)
	require.Contains(t, overrides, tokenRoute)
	assert.Nil(t, global, "the global cap is untouched when it was not named")

	configs, _ := DefaultPublicRateLimits()
	merged := MergePublicRateLimits(configs, overrides)

	assert.Equal(t, PublicRateLimitConfig{Rate: 20, Burst: 100}, merged[tokenRoute],
		"the named endpoint moves")
	assert.Equal(t, PublicRateLimitConfig{Rate: 5, Burst: 10}, merged["POST:/api/admin/auth/login"],
		"login is untouched - raising one endpoint must not lower the guard on the rest")
	assert.Equal(t, PublicRateLimitConfig{Rate: 10, Burst: 20}, merged["POST:/api/admin/gdpr/export"])
	assert.Len(t, merged, len(configs), "no route is dropped by an override")

	// The defaults the caller still holds are the defaults.
	assert.Equal(t, PublicRateLimitConfig{Rate: 5, Burst: 10}, configs[tokenRoute],
		"merging must not write back into the shipped table")
}

func TestPublicRateLimits_SeveralEntriesAndTheGlobalCap(t *testing.T) {
	overrides, global, err := ParsePublicRateLimits(
		[]string{hookRoute + "=20:100", " GET:/api/v1/flows/p/{flow_id}=9:11 "},
		"200:400",
	)
	require.NoError(t, err)

	assert.Equal(t, PublicRateLimitConfig{Rate: 20, Burst: 100}, overrides[hookRoute])
	assert.Equal(t, PublicRateLimitConfig{Rate: 9, Burst: 11}, overrides["GET:/api/v1/flows/p/{flow_id}"])
	assert.Equal(t, &PublicRateLimitConfig{Rate: 200, Burst: 400}, global)
}

// A limit may be lowered as well as raised: an operator who wants a tighter
// cap than the engine ships is exercising the same setting.
func TestPublicRateLimits_ALimitCanBeTightened(t *testing.T) {
	overrides, _, err := ParsePublicRateLimits([]string{"POST:/api/admin/auth/login=1:1"}, "")
	require.NoError(t, err)

	configs, _ := DefaultPublicRateLimits()
	merged := MergePublicRateLimits(configs, overrides)

	assert.Equal(t, PublicRateLimitConfig{Rate: 1, Burst: 1}, merged["POST:/api/admin/auth/login"])
}

// A typo must stop the boot. Accepting it and keeping the old limit would leave
// an operator believing a cap had moved when it had not.
func TestPublicRateLimits_MalformedEntryIsRejected(t *testing.T) {
	for _, bad := range []string{
		"no-equals-sign",
		hookRoute + "=",
		hookRoute + "=20",
		hookRoute + "=abc:100",
		hookRoute + "=20:abc",
		hookRoute + "=0:100",  // a zero rate refuses everything
		hookRoute + "=20:0",   // a zero burst refuses the first request
		hookRoute + "=-5:100", // negative is not "unlimited"
		"=20:100",
	} {
		overrides, global, err := ParsePublicRateLimits([]string{bad}, "")
		require.Error(t, err, "malformed entry must be rejected: %q", bad)
		assert.Contains(t, err.Error(), "invalid public rate limit")
		assert.Nil(t, overrides)
		assert.Nil(t, global)
	}

	_, _, err := ParsePublicRateLimits(nil, "not-a-limit")
	require.Error(t, err)
}

// One bad entry invalidates the setting rather than being skipped: half-applied
// limits are the state nobody can reason about.
func TestPublicRateLimits_OneBadEntryRejectsTheWholeSetting(t *testing.T) {
	overrides, _, err := ParsePublicRateLimits([]string{"garbage", hookRoute + "=20:100"}, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "garbage")
	assert.Nil(t, overrides)
}

// Blank entries come from a trailing comma in the environment, and mean nothing
// rather than being a mistake.
func TestPublicRateLimits_BlankEntriesAreIgnored(t *testing.T) {
	overrides, _, err := ParsePublicRateLimits([]string{"", "  ", hookRoute + "=20:100"}, "")

	require.NoError(t, err)
	require.Len(t, overrides, 1)
	assert.Equal(t, PublicRateLimitConfig{Rate: 20, Burst: 100}, overrides[hookRoute])
}

// The point of the setting: a burst the shipped cap refuses is admitted once an
// operator has raised it, from the same single address.
func TestPublicRateLimits_RaisedCapAdmitsABurstTheDefaultRefuses(t *testing.T) {
	const route = "POST:/api/admin/auth/login"
	shipped, _ := DefaultPublicRateLimits()

	countAdmitted := func(configs map[string]PublicRateLimitConfig, requests int) int {
		l := NewPublicEndpointRateLimiter(configs, nil, nil)
		defer l.Stop()
		admitted := 0
		for i := 0; i < requests; i++ {
			if allowed, _, _ := l.check(route+":198.51.100.7", configs[route].Rate, configs[route].Burst); allowed {
				admitted++
			}
		}
		return admitted
	}

	assert.Equal(t, 10, countAdmitted(shipped, 40),
		"the shipped burst of 10 is what an unconfigured deployment gets")

	overrides, _, err := ParsePublicRateLimits([]string{route + "=200:2000"}, "")
	require.NoError(t, err)
	assert.Equal(t, 40, countAdmitted(MergePublicRateLimits(shipped, overrides), 40),
		"the raised burst admits what the default refused")
}
