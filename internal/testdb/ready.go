package testdb

import "sync"

// Postgres clones each test database from a migrated template in one
// statement. MySQL and SQL Server have no template, so each test database is
// created and migrated from nothing, and on those dialects that is most of
// what a test costs. The tests of a package mostly run one at a time, so a
// few databases are prepared in the background while they run, and a test
// takes one that is ready instead of waiting for its own.

// readyAhead is how many databases a dialect prepares at once, and how many
// it holds ready.
const readyAhead = 2

// readyDB is a database prepared ahead of the test that takes it: created,
// migrated, and carrying the first ddl registrations. A failed preparation
// carries its error, and the name when the database was created.
type readyDB struct {
	name       string
	migrations int
	ddl        int
	err        error
}

// readyQueue prepares databases for one dialect.
type readyQueue struct {
	once sync.Once
	ch   chan readyDB
}

// start begins preparing databases in the background, once.
func (q *readyQueue) start(prepare func() readyDB) {
	q.once.Do(func() {
		q.ch = make(chan readyDB, readyAhead)
		for range readyAhead {
			// lyeve:bare-goroutine a panic while preparing must fail the test binary, not be logged and lost
			go func() {
				for {
					q.ch <- prepare()
				}
			}()
		}
	})
}

// take returns a prepared database, and starts the background preparation on
// first use. When none is ready it prepares one itself rather than wait, so
// tests running in parallel still prepare theirs in parallel.
func (q *readyQueue) take(prepare func() readyDB) readyDB {
	q.start(prepare)
	select {
	case r := <-q.ch:
		return r
	default:
		return prepare()
	}
}
