package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func lintScript(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "tools", name))
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("lint script not found at %s: %v", abs, err)
	}
	return abs
}

// TestLintNoRawTenantHeader verifies that lint-no-raw-tenant-header.sh:
//  1. Exits 0 on the clean repo.
//  2. Exits non-zero when a non-middleware Go file reads X-Tenant-ID directly.
//  3. Exits 0 when a middleware file uses it (allowed).
func TestLintNoRawTenantHeader(t *testing.T) {
	t.Parallel()

	script := lintScript(t, "lint-no-raw-tenant-header.sh")

	t.Run("clean_repo_passes", func(t *testing.T) {
		repoRoot, _ := filepath.Abs("..") // the repository root
		cmd := exec.Command("bash", script, repoRoot)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected clean repo to pass, got: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "OK: no raw") {
			t.Errorf("expected OK message, got: %s", out)
		}
	})

	t.Run("violation_caught", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "handler.go"), []byte(`package handler

import "net/http"

func handler(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get("X-Tenant-ID")
	_ = tenant
}
`), 0644)

		cmd := exec.Command("bash", script, dir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected violation to be caught, got exit 0:\n%s", out)
		}
		if !strings.Contains(string(out), "VIOLATION:") {
			t.Errorf("expected VIOLATION in output, got: %s", out)
		}
	})

	t.Run("middleware_allowed", func(t *testing.T) {
		dir := t.TempDir()
		midDir := filepath.Join(dir, "internal", "middleware")
		os.MkdirAll(midDir, 0755)
		os.WriteFile(filepath.Join(midDir, "tenant.go"), []byte(`package middleware

import "net/http"

func TenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := r.Header.Get("X-Tenant-ID")
		_ = tenant
		next.ServeHTTP(w, r)
	})
}
`), 0644)

		cmd := exec.Command("bash", script, dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected middleware read to be allowed, got: %v\n%s", err, out)
		}
	})

	t.Run("test_files_excluded", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "handler_test.go"), []byte(`package handler_test

import (
	"net/http"
	"testing"
)

func TestHandler(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("X-Tenant-ID", "test-tenant")
}
`), 0644)

		cmd := exec.Command("bash", script, dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected test file to be excluded, got: %v\n%s", err, out)
		}
	})
}

// TestLintNoRawError verifies that lint-no-raw-error-text.sh:
//  1. Exits 0 on the clean repo.
//  2. Exits non-zero when httpx.ErrorReq or http.Error passes err.Error().
//  3. Exits 0 when middleware files are excluded.
func TestLintNoRawError(t *testing.T) {
	t.Parallel()

	script := lintScript(t, "lint-no-raw-error-text.sh")

	t.Run("clean_repo_passes", func(t *testing.T) {
		repoRoot, _ := filepath.Abs("..") // the repository root
		cmd := exec.Command("bash", script, repoRoot)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected clean repo to pass, got: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "OK: no raw error") {
			t.Errorf("expected OK message, got: %s", out)
		}
	})

	t.Run("httpx_ErrorReq_caught", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "handler.go"), []byte(`package handler

import (
	"net/http"
	"strconv"
)

var httpx = struct {
	ErrorReq func(w http.ResponseWriter, r *http.Request, status int, msg string)
}{
	ErrorReq: func(w http.ResponseWriter, r *http.Request, status int, msg string) {
		http.Error(w, msg, status)
	},
}

func handler(w http.ResponseWriter, r *http.Request) {
	_, err := strconv.Atoi("bad")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, err.Error())
		return
	}
}
`), 0644)

		cmd := exec.Command("bash", script, dir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected violation to be caught, got exit 0:\n%s", out)
		}
		if !strings.Contains(string(out), "VIOLATION:") {
			t.Errorf("expected VIOLATION in output, got: %s", out)
		}
	})

	t.Run("http_Error_caught", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "handler.go"), []byte(`package handler

import (
	"errors"
	"net/http"
)

func handler(w http.ResponseWriter, r *http.Request) {
	err := errors.New("db connection refused")
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
`), 0644)

		cmd := exec.Command("bash", script, dir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected violation to be caught, got exit 0:\n%s", out)
		}
	})

	t.Run("static_message_allowed", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "handler.go"), []byte(`package handler

import (
	"net/http"
	"strconv"
)

var httpx = struct {
	ErrorReq func(w http.ResponseWriter, r *http.Request, status int, msg string)
}{
	ErrorReq: func(w http.ResponseWriter, r *http.Request, status int, msg string) {
		http.Error(w, msg, status)
	},
}

func handler(w http.ResponseWriter, r *http.Request) {
	_, err := strconv.Atoi("bad")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter")
		return
	}
}
`), 0644)

		cmd := exec.Command("bash", script, dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected static message to pass, got: %v\n%s", err, out)
		}
	})

	t.Run("middleware_excluded", func(t *testing.T) {
		dir := t.TempDir()
		midDir := filepath.Join(dir, "internal", "middleware")
		os.MkdirAll(midDir, 0755)
		os.WriteFile(filepath.Join(midDir, "decompress.go"), []byte(`package middleware

import (
	"fmt"
	"net/http"
)

func decompress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, fmt.Sprintf("decompress: %v", err), http.StatusBadRequest)
	})
}
`), 0644)

		cmd := exec.Command("bash", script, dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("expected middleware to be excluded, got: %v\n%s", err, out)
		}
	})
}

// TestLintValidateTags verifies that lint-validate-tags.sh:
//  1. Exits 0 on this repository.
//  2. Catches a size constraint on a bool, which panics the validator.
//  3. Catches a string-length cap sitting on a number, which caps the value.
//  4. Leaves a deliberate numeric range alone.
//  5. Refuses to pass on an empty scan rather than reporting a clean tree.
func TestLintValidateTags(t *testing.T) {
	t.Parallel()

	script := lintScript(t, "lint-validate-tags.sh")

	run := func(t *testing.T, fields string) ([]byte, error) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "tags")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		src := "package tags\n\ntype in struct {\n" + fields + "}\n"
		if err := os.WriteFile(filepath.Join(dir, "in.go"), []byte(src), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		return exec.Command("bash", script, dir).CombinedOutput()
	}

	t.Run("clean_repo_passes", func(t *testing.T) {
		repoRoot, _ := filepath.Abs("..")
		out, err := exec.Command("bash", script, repoRoot).CombinedOutput()
		if err != nil {
			t.Fatalf("expected the repository to be clean, got: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "none miscast") {
			t.Errorf("expected a clean summary, got: %s", out)
		}
	})

	t.Run("size_constraint_on_a_bool_caught", func(t *testing.T) {
		out, err := run(t, "\tEnabled *bool `json:\"enabled\" validate:\"omitempty,max=255\"`\n")
		if err == nil {
			t.Fatalf("expected the bool constraint to be caught, got exit 0:\n%s", out)
		}
		if !strings.Contains(string(out), "panics the validator") {
			t.Errorf("expected the panic to be named, got: %s", out)
		}
	})

	t.Run("string_length_on_a_number_caught", func(t *testing.T) {
		out, err := run(t, "\tStatusCode *int `json:\"status_code\" validate:\"omitempty,max=255\"`\n")
		if err == nil {
			t.Fatalf("expected the miscast cap to be caught, got exit 0:\n%s", out)
		}
		if !strings.Contains(string(out), "caps the value") {
			t.Errorf("expected the effect to be named, got: %s", out)
		}
	})

	t.Run("deliberate_range_passes", func(t *testing.T) {
		out, err := run(t,
			"\tPort int `json:\"port\" validate:\"omitempty,min=1,max=65535\"`\n"+
				"\tPercent int `json:\"percent\" validate:\"omitempty,max=100\"`\n"+
				"\tOn *bool `json:\"on\" validate:\"omitempty\"`\n")
		if err != nil {
			t.Fatalf("expected a chosen range to pass, got: %v\n%s", err, out)
		}
	})

	t.Run("empty_scan_is_not_a_pass", func(t *testing.T) {
		out, err := exec.Command("bash", script, t.TempDir()).CombinedOutput()
		if err == nil {
			t.Fatalf("expected an empty tree to fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(string(out), "Scanning nothing is not a pass") {
			t.Errorf("expected the refusal to be explained, got: %s", out)
		}
	})
}

// TestLintNoTestSeams verifies that lint-no-test-seams.sh catches each of the
// shapes a test-only switch takes, and leaves alone the environment reads
// that steer a test suite the sanctioned way.
func TestLintNoTestSeams(t *testing.T) {
	t.Parallel()

	script := lintScript(t, "lint-no-test-seams.sh")

	tree := func(t *testing.T, files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		pkg := filepath.Join(dir, "internal")
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(pkg, name), []byte(body), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		return dir
	}

	run := func(t *testing.T, dir string) (string, error) {
		t.Helper()
		out, err := exec.Command("bash", script, dir).CombinedOutput()
		return string(out), err
	}

	t.Run("clean_repo_passes", func(t *testing.T) {
		repoRoot, _ := filepath.Abs("..")
		out, err := run(t, repoRoot)
		if err != nil {
			t.Fatalf("expected the repo to pass, got: %v\n%s", err, out)
		}
		if !strings.Contains(out, "OK: no test seams") {
			t.Errorf("expected the OK message, got: %s", out)
		}
	})

	t.Run("env_bypass_caught", func(t *testing.T) {
		dir := tree(t, map[string]string{"guard.go": `package internal

import "os"

func skip() bool { return os.Getenv("GUARD_E2E_SKIP") == "true" }
`})
		out, err := run(t, dir)
		if err == nil {
			t.Fatalf("expected the env bypass to fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "[env]") {
			t.Errorf("expected the category to be named, got:\n%s", out)
		}
	})

	t.Run("request_knob_caught", func(t *testing.T) {
		dir := tree(t, map[string]string{"probe.go": `package internal

import "net/http"

func forced(r *http.Request) bool { return r.URL.Query().Get("force_db_fail") == "true" }
`})
		out, err := run(t, dir)
		if err == nil {
			t.Fatalf("expected the request knob to fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "[request]") {
			t.Errorf("expected the category to be named, got:\n%s", out)
		}
	})

	t.Run("test_variant_build_caught", func(t *testing.T) {
		dir := tree(t, map[string]string{"relax_e2e.go": "//go:build e2e\n\npackage internal\n"})
		out, err := run(t, dir)
		if err == nil {
			t.Fatalf("expected the test variant to fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "[tag]") {
			t.Errorf("expected the category to be named, got:\n%s", out)
		}
	})

	// The name is assembled at runtime, so reading it out of the Getenv call
	// finds nothing.
	t.Run("composed_name_caught", func(t *testing.T) {
		dir := tree(t, map[string]string{"client.go": `package internal

import "os"

func insecure(prefix string) string { return os.Getenv(prefix + "_TLS_INSECURE") }
`})
		out, err := run(t, dir)
		if err == nil {
			t.Fatalf("expected the composed name to fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "[literal]") {
			t.Errorf("expected the category to be named, got:\n%s", out)
		}
	})

	// An error code is not a switch, whatever word it contains.
	t.Run("constant_that_merely_reads_like_one_allowed", func(t *testing.T) {
		dir := tree(t, map[string]string{"codes.go": `package internal

const AccountDisabled = "AUTH_ACCOUNT_DISABLED"
const LoginSkipped = "AUTH_LOGIN_SKIPPED"
`})
		if out, err := run(t, dir); err != nil {
			t.Fatalf("expected the error codes to pass, got: %v\n%s", err, out)
		}
	})

	// A suite points the engine at a throwaway database and a chosen dialect.
	// Both are environment reads and neither weakens anything, so both pass.
	t.Run("steering_the_environment_allowed", func(t *testing.T) {
		dir := tree(t, map[string]string{"config.go": `package internal

import "os"

func dsn() string     { return os.Getenv("DATABASE_URL") }
func dialect() string { return os.Getenv("CI_DIALECT") }
func poolSize() string { return os.Getenv("POOL_DEFAULT_POOL_SIZE") }
`})
		if out, err := run(t, dir); err != nil {
			t.Fatalf("expected the steering reads to pass, got: %v\n%s", err, out)
		}
	})

	t.Run("justified_suppression_honored", func(t *testing.T) {
		dir := tree(t, map[string]string{"reader.go": `package internal

import "os"

//lyeve:allow-test-seam the reader picks its dialect before the pool exists
func dialect() string { return os.Getenv("READER_DIALECT_SKIP") }
`})
		if out, err := run(t, dir); err != nil {
			t.Fatalf("expected the justified suppression to pass, got: %v\n%s", err, out)
		}
	})

	// A control that relaxes when the production flag is off is a switch the
	// absence of APP_ENV flips. The three shapes are the negated condition,
	// the else arm of a positive one, and a compare against the raw variable.
	t.Run("production_keyed_anonymous_caller_caught", func(t *testing.T) {
		dir := tree(t, map[string]string{"auth.go": `package internal

func intercept(secrets []string, isProduction bool, ctx ctxT, req any, handler func(ctxT, any) (any, error)) (any, error) {
	devMode := len(secrets) == 0
	if devMode && !isProduction {
		return handler(withClaims(ctx, &claims{}), req)
	}
	return nil, errUnauthenticated
}
`})
		out, err := run(t, dir)
		if err == nil {
			t.Fatalf("expected the anonymous caller to fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "[production]") {
			t.Errorf("expected the category to be named, got:\n%s", out)
		}
	})

	t.Run("production_keyed_else_arm_caught", func(t *testing.T) {
		dir := tree(t, map[string]string{"tls.go": `package internal

func tlsFor(cfg cfgT, conf *tlsT) {
	if cfg.Bool("is_production") {
		logRefusal()
	} else {
		conf.InsecureSkipVerify = true
	}
}
`})
		out, err := run(t, dir)
		if err == nil {
			t.Fatalf("expected the else arm to fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "[production]") {
			t.Errorf("expected the category to be named, got:\n%s", out)
		}
	})

	t.Run("app_env_compare_caught", func(t *testing.T) {
		dir := tree(t, map[string]string{"limit.go": `package internal

import "os"

func limiter() limiterT {
	if os.Getenv("APP_ENV") != "production" {
		return noopLimiter{} // rate limit off outside production
	}
	return tokenBucket()
}
`})
		out, err := run(t, dir)
		if err == nil {
			t.Fatalf("expected the APP_ENV compare to fail, got exit 0:\n%s", out)
		}
		if !strings.Contains(out, "[production]") {
			t.Errorf("expected the category to be named, got:\n%s", out)
		}
	})

	// Registering an extra service, refusing to boot, and a production-only
	// validation pass are decisions about what exists, not about who gets in.
	t.Run("production_branch_that_weakens_nothing_allowed", func(t *testing.T) {
		dir := tree(t, map[string]string{"boot.go": `package internal

func (c *Config) ValidateProduction() error {
	if !c.IsProduction() {
		return nil
	}
	return c.checkSecrets()
}

func register(srv *serverT, isProduction bool) {
	if !isProduction {
		reflection.Register(srv)
	}
	if isProduction {
		srv.refuseWithoutSigningKey()
	}
}

func products(cfg cfgT, isProd bool) []string {
	if !isProd {
		return cfg.Strings("product_catalog")
	}
	return nil
}
`})
		if out, err := run(t, dir); err != nil {
			t.Fatalf("expected the harmless branches to pass, got: %v\n%s", err, out)
		}
	})

	t.Run("production_branch_with_justification_allowed", func(t *testing.T) {
		dir := tree(t, map[string]string{"tls.go": `package internal

func tlsFor(cfg cfgT, conf *tlsT) {
	if cfg.Bool("is_production") {
		logRefusal()
	//lyeve:allow-test-seam a self-signed development broker is a real deployment; production refuses it above
	} else {
		conf.InsecureSkipVerify = true
	}
}
`})
		if out, err := run(t, dir); err != nil {
			t.Fatalf("expected the justified branch to pass, got: %v\n%s", err, out)
		}
	})
}

// TestLintMySQLInlineReferences verifies that lint-mysql-inline-references.sh
// tells the two spellings of a foreign key apart.
//
// The distinction is the whole point of the gate: InnoDB creates a key from
// the table-level clause and silently creates none from the column-level one,
// while the other two dialects create one from either. A gate that cannot
// tell them apart is worse than none, because it would send someone to rewrite
// a key that already works.
func TestLintMySQLInlineReferences(t *testing.T) {
	t.Parallel()

	script := lintScript(t, "lint-mysql-inline-references.sh")

	// The script takes the tree to scan, which defaults to this repository.
	run := func(t *testing.T, migration string) ([]byte, error) {
		t.Helper()
		core := filepath.Join(t.TempDir(), "tree")
		dir := filepath.Join(core, "migrations", "mysql")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "001_fixture.up.sql"), []byte(migration), 0o644); err != nil {
			t.Fatalf("write migration: %v", err)
		}
		return exec.Command("bash", script, core).CombinedOutput()
	}

	t.Run("column_level_reference_caught", func(t *testing.T) {
		out, err := run(t, "CREATE TABLE a (\n  id CHAR(36) PRIMARY KEY,\n"+
			"  owner_id CHAR(36) REFERENCES b(id) ON DELETE SET NULL\n) ENGINE=InnoDB;\n")
		if err == nil {
			t.Fatalf("expected a failure on a column-level key, got:\n%s", out)
		}
		if !strings.Contains(string(out), "owner_id") {
			t.Errorf("expected the offending line in the output, got:\n%s", out)
		}
	})

	t.Run("table_level_reference_accepted", func(t *testing.T) {
		out, err := run(t, "CREATE TABLE a (\n  id CHAR(36) PRIMARY KEY,\n  owner_id CHAR(36),\n"+
			"  FOREIGN KEY (owner_id) REFERENCES b(id) ON DELETE SET NULL\n) ENGINE=InnoDB;\n")
		if err != nil {
			t.Fatalf("expected a table-level key to pass, got: %v\n%s", err, out)
		}
	})

	t.Run("named_constraint_accepted", func(t *testing.T) {
		out, err := run(t, "ALTER TABLE a ADD CONSTRAINT fk_a_owner\n"+
			"  FOREIGN KEY (owner_id) REFERENCES b(id) ON DELETE CASCADE;\n")
		if err != nil {
			t.Fatalf("expected a named constraint to pass, got: %v\n%s", err, out)
		}
	})

	t.Run("reference_in_a_comment_ignored", func(t *testing.T) {
		out, err := run(t, "-- owner_id REFERENCES b(id): added in 002 as a table constraint\n"+
			"CREATE TABLE a (id CHAR(36) PRIMARY KEY) ENGINE=InnoDB;\n")
		if err != nil {
			t.Fatalf("expected prose to pass, got: %v\n%s", err, out)
		}
	})

	t.Run("scanning_nothing_is_not_a_pass", func(t *testing.T) {
		ws := t.TempDir()
		core := filepath.Join(ws, "tree")
		if err := os.MkdirAll(core, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		out, err := exec.Command("bash", script, core).CombinedOutput()
		if err == nil {
			t.Fatalf("a tree with no migrations must not read as clean, got:\n%s", out)
		}
	})
}

// TestLintMySQLIndexGuard verifies that lint-mysql-index-guard.sh reports a
// CREATE INDEX that runs as a statement of its own and passes the two forms
// that re-run cleanly on MySQL.
func TestLintMySQLIndexGuard(t *testing.T) {
	t.Parallel()

	script := lintScript(t, "lint-mysql-index-guard.sh")

	run := func(t *testing.T, migration string) ([]byte, error) {
		t.Helper()
		repo := filepath.Join(t.TempDir(), "tree")
		dir := filepath.Join(repo, "migrations", "mysql")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "001_fixture.up.sql"), []byte(migration), 0o644); err != nil {
			t.Fatalf("write migration: %v", err)
		}
		return exec.Command("bash", script, repo).CombinedOutput()
	}

	t.Run("clean_repo_passes", func(t *testing.T) {
		repoRoot, _ := filepath.Abs("..")
		out, err := exec.Command("bash", script, repoRoot).CombinedOutput()
		if err != nil {
			t.Fatalf("expected this repository to pass, got: %v\n%s", err, out)
		}
	})

	for name, migration := range map[string]string{
		"bare_index":        "CREATE TABLE IF NOT EXISTS a (id CHAR(36) PRIMARY KEY);\nCREATE INDEX idx_a_id ON a (id);\n",
		"bare_unique_index": "CREATE TABLE IF NOT EXISTS a (id CHAR(36));\n\n  create unique index idx_a_id\n    ON a (id);\n",
		"if_not_exists":     "CREATE INDEX IF NOT EXISTS idx_a_id ON a (id);\n",
	} {
		t.Run(name+"_caught", func(t *testing.T) {
			out, err := run(t, migration)
			if err == nil {
				t.Fatalf("expected a failure, got:\n%s", out)
			}
			if !strings.Contains(string(out), "001_fixture.up.sql:") || !strings.Contains(string(out), "idx_a_id") {
				t.Errorf("expected the file, line and statement in the output, got:\n%s", out)
			}
		})
	}

	for name, migration := range map[string]string{
		"key_clause": "CREATE TABLE IF NOT EXISTS a (\n  id CHAR(36) PRIMARY KEY,\n  KEY idx_a_id (id)\n);\n",
		"prepared": "SET @n = (SELECT COUNT(*) FROM information_schema.statistics WHERE index_name = 'idx_a_id');\n" +
			"SET @s = IF(@n = 0, 'CREATE INDEX idx_a_id ON a (id)', 'SELECT 1');\n" +
			"PREPARE st FROM @s;\nEXECUTE st;\nDEALLOCATE PREPARE st;\n",
		"comment": "-- CREATE INDEX idx_a_id ON a (id) would not re-run.\n/* CREATE INDEX idx_b ON a (id); */\n" +
			"CREATE TABLE IF NOT EXISTS a (id CHAR(36) PRIMARY KEY);\n",
	} {
		t.Run(name+"_accepted", func(t *testing.T) {
			out, err := run(t, migration)
			if err != nil {
				t.Fatalf("expected a pass, got: %v\n%s", err, out)
			}
		})
	}

	t.Run("scanning_nothing_is_not_a_pass", func(t *testing.T) {
		out, err := exec.Command("bash", script, t.TempDir()).CombinedOutput()
		if err == nil {
			t.Fatalf("a tree with no migrations must not read as clean, got:\n%s", out)
		}
	})
}
