package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// settingsRowWriter is one of the writers that read the tenant's engine
// settings row, change their own key and write the whole row back.
type settingsRowWriter struct {
	name string
	key  string
	want any
	run  func(ctx context.Context) error
}

func settingsRowWriters(pool db.DB, store *db.PluginConfigStore) []settingsRowWriter {
	host := testhost.New(pool)
	serve := func(ctx context.Context, h http.HandlerFunc, path, body string) error {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			return fmt.Errorf("PUT %s answered %d: %s", path, rec.Code, rec.Body.String())
		}
		return nil
	}
	windows := &providerWindowHandler{store: store, windows: config.NewProviderWindows(pool), host: host}
	section := newSettingsSection(store, nil, pool.Engine())
	return []settingsRowWriter{
		{
			name: "settings save", key: "JWT_EXPIRY_SECS", want: "600",
			run: func(ctx context.Context) error {
				return serve(ctx, configSaveHandler(store, nil), "/api/admin/config", `{"values":{"JWT_EXPIRY_SECS":"600"}}`)
			},
		},
		{
			name: "provider window", key: core.ProviderWindowSettingKey, want: float64(30),
			run: func(ctx context.Context) error {
				return serve(ctx, windows.put, "/api/admin/auth/provider-window", `{"days":30}`)
			},
		},
		{
			// The bundle apply runs in a transaction of its own, the way the
			// configuration sync runs every section of a step in one.
			name: "bundle apply", key: "RATE_LIMIT_RPS", want: "250",
			run: func(ctx context.Context) error {
				return db.WriteInTransaction(ctx, pool, func(ctx context.Context) error {
					_, err := section.ApplyConfig(ctx, host.Querier(ctx), json.RawMessage(`{"RATE_LIMIT_RPS":"250"}`), core.ConfigOptions{})
					return err
				})
			},
		},
	}
}

// Two writers of different keys overlap: the first has written its key and
// holds its transaction open while the second saves another key. Once the
// first commits, the row holds both keys, whether or not it existed before.
// Without a lock the second writer reads the row as it was before the first
// wrote, and its write puts that row back, dropping the first writer's key.
func TestSettingsRow_OverlappingWritersOfDifferentKeysBothLand(t *testing.T) {
	withResolver(t, nil)
	for name, open := range settingsPools(t) {
		t.Run(name, func(t *testing.T) {
			pool := open(t)
			store := db.NewPluginConfigStore(pool).WithSealer(db.NewConfigSealer("test-master-key-for-settings-row-writers"))
			ctx := core.WithTenantID(context.Background(), "default")
			actor := uuid.NewString()
			ctx = context.WithValue(ctx, auth.ClaimsKey, &auth.Claims{UserID: actor, Roles: []string{"super_admin"}, TenantID: "default"})
			ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: actor, Roles: []string{"super_admin"}, TenantID: "default"})

			writers := settingsRowWriters(pool, store)
			for _, first := range writers {
				for _, second := range writers {
					if first.name == second.name {
						continue
					}
					for _, existing := range []bool{true, false} {
						t.Run(fmt.Sprintf("%s then %s, row exists %v", first.name, second.name, existing), func(t *testing.T) {
							require.NoError(t, store.Delete(ctx, config.CoreSettingsOwner))
							if existing {
								require.NoError(t, store.Set(ctx, config.CoreSettingsOwner, map[string]any{
									"CORS_ORIGINS":     "https://www.example.com",
									"EXAMPLE_PASSWORD": "kept-secret",
								}, nil))
							}

							wrote, release, firstDone := make(chan error, 1), make(chan struct{}), make(chan error, 1)
							go func() {
								firstDone <- db.WriteInTransaction(ctx, pool, func(ctx context.Context) error {
									err := first.run(ctx)
									wrote <- err
									if err != nil {
										return err
									}
									<-release
									return nil
								})
							}()
							require.NoError(t, <-wrote)

							secondDone := make(chan error, 1)
							go func() { secondDone <- second.run(ctx) }()

							// The second writer is given time to reach the row. It must
							// still be waiting: the first writer's change is not
							// committed, and a write made now would be made without it.
							var secondErr error
							waited := true
							select {
							case secondErr = <-secondDone:
								waited = false
							case <-time.After(300 * time.Millisecond):
							}
							close(release)
							require.NoError(t, <-firstDone)
							if waited {
								secondErr = <-secondDone
							}
							require.NoError(t, secondErr)
							assert.True(t, waited, "%s did not wait for the open write of %s", second.name, first.name)

							got, err := store.GetResolved(ctx, config.CoreSettingsOwner)
							require.NoError(t, err)
							assert.Equal(t, first.want, got[first.key], "%s lost the key %s wrote first", second.name, first.name)
							assert.Equal(t, second.want, got[second.key], "the key %s wrote second is missing", second.name)
							if existing {
								assert.Equal(t, "https://www.example.com", got["CORS_ORIGINS"], "a key neither writer touched was dropped")
								assert.Equal(t, "kept-secret", got["EXAMPLE_PASSWORD"], "a sealed credential did not survive the rewrite")
							}

							row, err := pool.QueryRow(ctx,
								`SELECT COUNT(*) FROM sys_plugin_config WHERE tenant_id = $1 AND plugin_name = $2`,
								"default", config.CoreSettingsOwner)
							require.NoError(t, err)
							var rows int
							require.NoError(t, row.Scan(&rows))
							assert.Equal(t, 1, rows)
						})
					}
				}
			}
		})
	}
}

// The three writers race: each runs twice at once, all six released together,
// over rounds that alternate between a stored row and none. Every round ends
// with every writer's key in the row and the keys none of them touched still
// there, and no write fails. Without the row lock a writer writes back a row
// read before another's write, which this loses a few dozen keys to on every
// dialect, SQL Server included, where the ordered test above cannot lose one.
//
// The pool holds twelve connections, so the six writers can all be in the
// database at once, and the test asserts that six were.
func TestSettingsRow_RacingWritersLoseNoKey(t *testing.T) {
	withResolver(t, nil)
	const rounds, copies, conns = 60, 2, 12
	for name, open := range map[string]func(*testing.T, int) db.DB{
		"postgres": testdb.PostgresWithMaxConns, "mysql": testdb.MySQLWithMaxConns, "mssql": testdb.MSSQLWithMaxConns,
	} {
		if !testdb.ShouldTest(name) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			pool := open(t, conns)
			store := db.NewPluginConfigStore(pool).WithSealer(db.NewConfigSealer("test-master-key-for-settings-row-writers"))
			ctx := core.WithTenantID(context.Background(), "default")
			actor := uuid.NewString()
			ctx = context.WithValue(ctx, auth.ClaimsKey, &auth.Claims{UserID: actor, Roles: []string{"super_admin"}, TenantID: "default"})
			ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: actor, Roles: []string{"super_admin"}, TenantID: "default"})
			writers := settingsRowWriters(pool, store)

			stop, sampled := make(chan struct{}), make(chan int, 1)
			go func() {
				peak := 0
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						sampled <- peak
						return
					case <-tick.C:
						peak = max(peak, pool.Stats().InUse)
					}
				}
			}()

			lost, failed := 0, 0
			var firstFailure error
			for round := range rounds {
				existing := round%2 == 0
				require.NoError(t, store.Delete(ctx, config.CoreSettingsOwner))
				if existing {
					require.NoError(t, store.Set(ctx, config.CoreSettingsOwner, map[string]any{
						"CORS_ORIGINS":     "https://www.example.com",
						"EXAMPLE_PASSWORD": "kept-secret",
					}, nil))
				}

				start := make(chan struct{})
				errs := make(chan error, len(writers)*copies)
				var wg sync.WaitGroup
				for _, w := range writers {
					for range copies {
						wg.Go(func() {
							<-start
							errs <- w.run(ctx)
						})
					}
				}
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						failed++
						if firstFailure == nil {
							firstFailure = err
						}
					}
				}

				got, err := store.GetResolved(ctx, config.CoreSettingsOwner)
				require.NoError(t, err)
				for _, w := range writers {
					if got[w.key] != w.want {
						lost++
					}
				}
				if existing {
					for key, want := range map[string]any{"CORS_ORIGINS": "https://www.example.com", "EXAMPLE_PASSWORD": "kept-secret"} {
						if got[key] != want {
							lost++
						}
					}
				}
			}
			close(stop)

			assert.Zero(t, failed, "%d writes failed, the first with: %v", failed, firstFailure)
			assert.Zero(t, lost, "%d keys were lost over %d rounds", lost, rounds)
			assert.GreaterOrEqual(t, <-sampled, len(writers)*copies, "the writers never held their sessions at once, so they did not race")
		})
	}
}
