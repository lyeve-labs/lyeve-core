package dialect_test

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
)

func TestMust_Success(t *testing.T) {
	d := dialect.Must("postgres")
	if d.Name() != "postgres" {
		t.Errorf("Must(postgres): got %q, want postgres", d.Name())
	}

	d2 := dialect.Must("mysql")
	if d2.Name() != "mysql" {
		t.Errorf("Must(mysql): got %q, want mysql", d2.Name())
	}

	d3 := dialect.Must("")
	if d3.Name() != "postgres" {
		t.Errorf("Must(\"\"): got %q, want postgres", d3.Name())
	}
}

func TestMust_Panic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Must(unknown) should panic")
		}
	}()
	dialect.Must("oracle")
}
