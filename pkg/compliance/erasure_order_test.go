package compliance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingEraser notes the order it was called in through a shared slice.
type recordingEraser struct {
	name  string
	order *[]string
}

func (r *recordingEraser) EraseSubject(context.Context, string) (int64, error) {
	*r.order = append(*r.order, r.name)
	return 1, nil
}

// accountRecorder is the account eraser: the one that rewrites the record the
// others resolve the subject through.
type accountRecorder struct{ recordingEraser }

func (a *accountRecorder) ErasesAccount() {}

// The engine registers the account eraser before the plugin activator runs,
// so registration order puts it first. It must still be called last.
func TestErasers_AccountEraserRunsLastHoweverItRegistered(t *testing.T) {
	var order []string
	reg := &SubjectEraserRegistry{}

	reg.Register(&accountRecorder{recordingEraser{name: "account", order: &order}})
	reg.Register(&recordingEraser{name: "oauth", order: &order})
	reg.Register(&recordingEraser{name: "logging", order: &order})

	_, err := runSubjectErasureOn(context.Background(), reg, "someone@example.test")
	require.NoError(t, err)

	assert.Equal(t, []string{"oauth", "logging", "account"}, order,
		"a plugin resolves the subject through the account record, so the eraser that rewrites it goes last")
}

// The plugins keep the order they registered in, so this changes nothing for
// a registry with no account eraser in it.
func TestErasers_EverythingElseKeepsRegistrationOrder(t *testing.T) {
	var order []string
	reg := &SubjectEraserRegistry{}

	reg.Register(&recordingEraser{name: "a", order: &order})
	reg.Register(&recordingEraser{name: "b", order: &order})
	reg.Register(&recordingEraser{name: "c", order: &order})

	_, err := runSubjectErasureOn(context.Background(), reg, "someone@example.test")
	require.NoError(t, err)

	assert.Equal(t, []string{"a", "b", "c"}, order)
}

// resolvingEraser is the shape every plugin eraser that takes an address has:
// it looks the subject up in the account record, and finds nothing once that
// record has been rewritten.
type resolvingEraser struct {
	accounts *map[string]string // email -> user id
	erased   *int
}

func (r *resolvingEraser) EraseSubject(_ context.Context, identifier string) (int64, error) {
	if _, ok := (*r.accounts)[identifier]; !ok {
		return 0, nil
	}
	*r.erased++
	return 1, nil
}

// anonymizingAccount stands in for the sys_users eraser: it replaces the
// address with a sentinel, which is what takes the key away.
type anonymizingAccount struct {
	accounts *map[string]string
}

func (a *anonymizingAccount) EraseSubject(_ context.Context, identifier string) (int64, error) {
	if _, ok := (*a.accounts)[identifier]; !ok {
		return 0, nil
	}
	delete(*a.accounts, identifier)
	return 1, nil
}

func (a *anonymizingAccount) ErasesAccount() {}

// The failure mode, stated as the thing that goes wrong rather than as an
// ordering assertion: with the account eraser first, a plugin that resolves
// an address erases nothing and reports zero, which reads exactly like a
// person with no data in that plugin.
func TestErasure_APluginThatResolvesTheAddressStillFindsIt(t *testing.T) {
	accounts := map[string]string{"someone@example.test": "user-1"}
	erased := 0

	reg := &SubjectEraserRegistry{}
	reg.Register(&anonymizingAccount{accounts: &accounts})
	reg.Register(&resolvingEraser{accounts: &accounts, erased: &erased})

	rows, err := runSubjectErasureOn(context.Background(), reg, "someone@example.test")
	require.NoError(t, err)

	assert.Equal(t, 1, erased,
		"the plugin ran after the account was anonymized and could no longer resolve the subject")
	assert.EqualValues(t, 2, rows, "both erasers did work, so both are counted")
}
