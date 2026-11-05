package leakcheck

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

// unwritableProfile fails the way a profile the runtime cannot produce does.
type unwritableProfile struct{}

func (unwritableProfile) WriteTo(io.Writer, int) error { return errors.New("no profile") }
func (unwritableProfile) Count() int                   { return 0 }

// A check that cannot read the profile has found nothing, so it must fail the
// run rather than report no leaks.
func TestFind_FailsWhenTheProfileCannotBeWritten(t *testing.T) {
	_, err := find(unwritableProfile{}, io.Discard)
	assert.Error(t, err)
}
