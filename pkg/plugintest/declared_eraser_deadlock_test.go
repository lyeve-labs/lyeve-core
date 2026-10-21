package plugintest_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// victimHost answers the first catalog read the way SQL Server answers a
// statement it chose as a deadlock victim.
type victimHost struct {
	core.Host
	victims atomic.Int32
}

func (h *victimHost) Querier(ctx context.Context) core.Querier {
	return &victimQuerier{Querier: h.Host.Querier(ctx), host: h}
}

type victimQuerier struct {
	core.Querier
	host *victimHost
}

func (q *victimQuerier) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	if strings.Contains(sql, "information_schema") && q.host.victims.Add(-1) >= 0 {
		return nil, mssql.Error{Number: 1205, Message: "Transaction was deadlocked on lock resources with another process and has been chosen as the deadlock victim. Rerun the transaction."}
	}
	return q.Querier.Query(ctx, sql, args...)
}

// A declared eraser reads the catalog before it erases. On SQL Server that
// read takes shared locks on catalog rows that a schema change running at the
// same time locks exclusively, and the server can choose the read as the
// deadlock victim, so the erasure reported an error under load. The read
// changes nothing and runs outside any transaction, so it is read again.
func TestDeclaredEraser_ReadsTheCatalogAgainAfterADeadlock(t *testing.T) {
	base := plugintest.MSSQL(t)
	ctx := context.Background()
	q := base.Querier(ctx)

	table := uniqueTable("deadlocked_people_")
	_, err := q.Exec(ctx, `CREATE TABLE `+table+` (id VARCHAR(36) PRIMARY KEY, email VARCHAR(255) NOT NULL, tenant_id VARCHAR(64) NULL)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+table) })
	_, err = q.Exec(ctx, `INSERT INTO `+table+` (id, email) VALUES ('1', 'ada@example.com')`)
	require.NoError(t, err)

	host := &victimHost{Host: base}
	host.victims.Store(1)
	called := false
	e := compliance.NewDeclaredEraser("deadlocked", host, compliance.SubjectErasure{
		Tables:  []string{table},
		Columns: []string{table + ".tenant_id"},
		Erase:   eraseByEmail(table, &called),
	})
	require.NotNil(t, e)

	n, err := e.EraseSubject(ctx, "ada@example.com")
	require.NoError(t, err, "a catalog read chosen as a deadlock victim failed the erasure")
	assert.True(t, called, "the erasure never reached its table")
	assert.EqualValues(t, 1, n)
}
