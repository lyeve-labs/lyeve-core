package testdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SQL Server refuses a CREATE DATABASE that meets the model database's lock
// with 1807 followed by 1802, and the driver numbers the error 1802, so the
// retry has to read every message of the batch to see it.
func TestMSSQLModelIsLocked_ReadsEveryMessageOfTheBatch(t *testing.T) {
	locked := mssql.Error{Number: 1807, Message: "Could not obtain exclusive lock on database 'model'. Retry the operation later."}
	failed := mssql.Error{Number: 1802, Message: "CREATE DATABASE failed. Some file names listed could not be created. Check related errors."}
	createRefused := failed
	createRefused.All = []mssql.Error{locked, failed}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"the driver's error for the refused create", createRefused, true},
		{"the same error wrapped", fmt.Errorf("create: %w", createRefused), true},
		{"the lock alone", locked, true},
		{"a create that failed for another reason", failed, false},
		{"an error from outside the driver", errors.New("dial tcp: connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mssqlModelIsLocked(tc.err))
		})
	}
}

// A session with model as its current database holds the lock a CREATE
// DATABASE needs. SQL Server waits about 20 seconds on its own before it
// refuses, so the hold outlasts that and only the retry lets the create
// through once the session moves away.
func TestCreateMSSQLDatabase_WaitsWhileModelIsInUse(t *testing.T) {
	if !ShouldTest("mssql") {
		t.Skip("CI_DIALECT != mssql")
	}
	ensureMSSQL(t)
	ctx := context.Background()
	admin, err := sql.Open("sqlserver", mssqlShared.adminDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	holder, err := admin.Conn(ctx)
	require.NoError(t, err)
	_, err = holder.ExecContext(ctx, "USE model")
	require.NoError(t, err)
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(25 * time.Second)
		_, _ = holder.ExecContext(ctx, "USE master")
		_ = holder.Close()
	}()
	t.Cleanup(func() { <-released })

	name := nextDBName("mssqlmodel")
	start := time.Now()
	err = createMSSQLDatabase(ctx, admin, name)
	t.Logf("create took %s", time.Since(start))
	require.NoError(t, err, "the create gave up while model was in use")
	_, _ = admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE [%s]", name))
}
