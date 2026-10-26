package compliance

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Clearing a subject's text by key
//
// SQL selects the rows whose text may name the subject and Go confirms each,
// because a contains also takes a longer address and the MySQL collations
// read josé@ as jose@. The clear then names every confirmed row by its key
// and by the texts Go confirmed, so a row edited between the read and the
// clear keeps its edit.

// EraseParams is how many parameters one clear binds at most, under the 2100
// SQL Server accepts.
const EraseParams = 2000

// ErrChangeTooWide is the refusal of a change whose arguments leave no room
// under EraseParams for one row's key and texts.
var ErrChangeTooWide = errors.New("change binds too many arguments for one row")

// ConfirmedRow is a row a read selected and Go confirmed: its key and its
// texts exactly as read, NULL kept apart from empty.
type ConfirmedRow[K string | uuid.UUID] struct {
	Key   K
	Texts []sql.NullString
}

// ConfirmedRows runs query, which selects a key and then texts columns, and
// returns the rows keep accepts. keep reads a NULL column as empty. A
// uuid.UUID key is read through core.ScanUUID, because SQL Server returns a
// UNIQUEIDENTIFIER in its own byte order.
func ConfirmedRows[K string | uuid.UUID](ctx context.Context, q core.Querier, dialect, query string, args []any, texts int, keep func([]string) bool) ([]ConfirmedRow[K], error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []ConfirmedRow[K]
	for rows.Next() {
		var key K
		dest := []any{&key}
		if id, ok := any(&key).(*uuid.UUID); ok {
			dest[0] = core.ScanUUID(dialect, id)
		}
		values := make([]sql.NullString, texts)
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, err
		}
		read := make([]string, texts)
		for i, v := range values {
			read[i] = v.String
		}
		if keep(read) {
			out = append(out, ConfirmedRow[K]{Key: key, Texts: values})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// KeyedChange is a clear ChangeByKey makes to confirmed rows.
type KeyedChange struct {
	// Change is an UPDATE that ends where a condition can follow: in WHERE,
	// or in AND after the clause of its read. Each batch completes it with
	// its rows.
	Change string
	// Args bind the placeholders Change holds.
	Args []any
	// Key is the key column. It names one row in the whole table, such as
	// its primary key, because the change adds no tenant clause of its own. A
	// caller scoped to one tenant puts that clause in Change.
	Key string
	// KeyCast is the type each key is cast to on SQL Server, or empty. The
	// driver sends a Go string as NVARCHAR, which outranks a CHAR column, so
	// uncast the column is converted on every row and the batch scans the
	// table.
	KeyCast string
	// Columns are the text columns, in the order of each row's Texts.
	Columns []string
}

// ChangeByKey makes c to each of rows whose key column still holds its key
// and whose columns still hold its texts, as many rows at a time as
// EraseParams allows. Each row's key and non-NULL texts bind the placeholders
// after c.Args. A text compares under a binary collation, because a
// case-insensitive or accent-insensitive one would pass an edit Go never
// confirmed. The change waits for the read to close, because both run on the
// one connection a request holds. A change whose c.Args leave no room for one
// row is refused with ErrChangeTooWide before any statement runs.
func ChangeByKey[K string | uuid.UUID](ctx context.Context, q core.Querier, dialect string, c KeyedChange, rows []ConfirmedRow[K]) (int64, error) {
	per := (EraseParams - len(c.Args)) / (1 + len(c.Columns))
	if per < 1 {
		return 0, ErrChangeTooWide
	}
	var total int64
	for len(rows) > 0 {
		batch := rows[:min(len(rows), per)]
		rows = rows[len(batch):]
		bound := append([]any{}, c.Args...)
		terms := make([]string, len(batch))
		for i, row := range batch {
			bound = append(bound, row.Key)
			mark := "$" + strconv.Itoa(len(bound))
			if c.KeyCast != "" && dialect == "mssql" {
				mark = "CAST(" + mark + " AS " + c.KeyCast + ")"
			}
			conds := []string{c.Key + " = " + mark}
			for j, column := range c.Columns {
				if !row.Texts[j].Valid {
					conds = append(conds, column+" IS NULL")
					continue
				}
				bound = append(bound, row.Texts[j].String)
				conds = append(conds, BinaryEquals(dialect, column, "$"+strconv.Itoa(len(bound))))
			}
			terms[i] = "(" + strings.Join(conds, " AND ") + ")"
		}
		tag, err := q.Exec(ctx, c.Change+"("+strings.Join(terms, " OR ")+")", bound...)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected
	}
	return total, nil
}

// BinaryEquals compares column to placeholder under a binary collation.
// PostgreSQL's default collations are deterministic, so its = already does.
// utf8mb4_bin and Latin1_General_100_BIN2 pad with spaces, so a value that
// differs only by trailing spaces compares equal on MySQL and SQL Server. A
// clear that takes such an edit is right, because the text still names the
// subject.
func BinaryEquals(dialect, column, placeholder string) string {
	switch dialect {
	case "mysql":
		return column + " COLLATE utf8mb4_bin = " + placeholder
	case "mssql":
		return column + " COLLATE Latin1_General_100_BIN2 = " + placeholder
	default:
		return column + " = " + placeholder
	}
}
