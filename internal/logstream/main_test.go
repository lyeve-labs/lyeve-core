package logstream_test

import (
	"os"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/leakcheck"
)

func TestMain(m *testing.M) {
	os.Exit(leakcheck.Main(m))
}
