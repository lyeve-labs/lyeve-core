package sqlx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	mssql "github.com/microsoft/go-mssqldb"
)

func TestIsRetryableTxError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"pg serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"pg deadlock detected", &pgconn.PgError{Code: "40P01"}, true},
		{"pg unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"mysql deadlock", &mysql.MySQLError{Number: 1213}, true},
		{"mysql lock wait timeout", &mysql.MySQLError{Number: 1205}, true},
		{"mysql duplicate entry", &mysql.MySQLError{Number: 1062}, false},
		{"mssql deadlock victim", mssql.Error{Number: 1205}, true},
		{"mssql pk violation", mssql.Error{Number: 2627}, false},
		// The callers wrap with %w before this ever sees the error.
		{"wrapped pg deadlock", fmt.Errorf("update counter: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"wrapped mssql victim", fmt.Errorf("update counter: %w", mssql.Error{Number: 1205}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableTxError(tc.err); got != tc.want {
				t.Errorf("IsRetryableTxError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryOnTxConflict(t *testing.T) {
	deadlock := mssql.Error{Number: 1205}

	t.Run("returns on first success without retrying", func(t *testing.T) {
		calls := 0
		err := RetryOnTxConflict(context.Background(), "op", func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})

	t.Run("retries a deadlock victim until it lands", func(t *testing.T) {
		calls := 0
		err := RetryOnTxConflict(context.Background(), "op", func() error {
			calls++
			if calls < 3 {
				return deadlock
			}
			return nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3", calls)
		}
	})

	t.Run("does not retry an error the engine will not fix", func(t *testing.T) {
		calls := 0
		want := errors.New("syntax error")
		err := RetryOnTxConflict(context.Background(), "op", func() error {
			calls++
			return want
		})
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want %v", err, want)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})

	t.Run("gives up after the attempt budget and keeps the cause", func(t *testing.T) {
		calls := 0
		err := RetryOnTxConflict(context.Background(), "assign bucket", func() error {
			calls++
			return deadlock
		})
		if err == nil {
			t.Fatal("err = nil, want failure")
		}
		if calls != DefaultTxAttempts {
			t.Errorf("calls = %d, want %d", calls, DefaultTxAttempts)
		}
		var msErr mssql.Error
		if !errors.As(err, &msErr) {
			t.Errorf("err = %v, want the driver error preserved", err)
		}
	})

	t.Run("abandons the retry when the context is done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := RetryOnTxConflict(ctx, "op", func() error {
			calls++
			cancel()
			return deadlock
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})
}
