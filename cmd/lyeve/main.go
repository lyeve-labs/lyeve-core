package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/runtime"
	"github.com/lyeve-labs/lyeve-libs/licenseprovider"
	"github.com/lyeve-labs/lyeve-libs/orphanpurge"
	"github.com/lyeve-labs/lyeve-libs/schemacarry"

	// Plugins compiled into this build
	_ "github.com/lyeve-labs/lyeve-plugin-ab-testing/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-ai/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-analytics/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-apianalytics/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-apikey/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-audit/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-bulk-import/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-cache/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-captcha/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-cluster/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-content/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-cron/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-data-export/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-data-residency/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-device-fingerprint/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-email/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-error-tracking/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-events/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-flow/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-goroutine-engine/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-graphql/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-grpc/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-idempotency/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-localization/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-logging/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-magic-link/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-media/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-messagebroker/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-mfa/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-multitenant/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-oauth/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-password-reset/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-permissions/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-pii-mask/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-profiler/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-query-monitor/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-rate-limit/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-realtime/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-recommendations/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-request-capture/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-review/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-saml/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-schema/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-scim/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-search/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-storage/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-synthetic-monitoring/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-telemetry/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-usage/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-waf/plugin"
	_ "github.com/lyeve-labs/lyeve-plugin-webhook/plugin"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

var expectedPlugins = []string{
	"ab-testing", "data-export",
	"synthetic-monitoring",
	"email", "graphql", "grpc", "multitenant", "webhook",
	"ai", "analytics", "apianalytics", "apikey", "audit", "bulk-import", "cache", "captcha", "cluster", "content", "cron", "device-fingerprint", "error-tracking", "events",
	"flow", "goroutine-engine", "idempotency", "profiler",
	"logging", "localization", "magic-link", "media", "messagebroker", "mfa", "oauth", "password-reset", "pii-mask", "query-monitor",
	"rate-limit", "realtime", "recommendations", "request-capture", "review", "usage",
	"saml",
	"schema", "scim", "search", "storage", "telemetry", "waf",
	"data-residency",
}

func main() {
	// Container HEALTHCHECK probes its own /healthz and exits before engine boot.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runtime.Healthcheck()
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	logger.Info("starting lyeve cms", "version", version, "commit", commit, "build_date", buildDate)

	registered := core.RegisteredPlugins()
	if missing := missingPlugins(expectedPlugins, registered); len(missing) > 0 {
		logger.Error("build is missing expected plugins: refusing to start",
			"missing", missing, "registered", registered, "expected", expectedPlugins)
		fmt.Fprintf(os.Stderr, "binary is missing plugins: %v\n", missing)
		os.Exit(2)
	}
	logger.Info("plugin registry resolved", "compiled", registered, "count", len(registered))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// This build links its own licensing implementation, before Run reads the
	// license. A build that links none runs on licensing.Open.
	if !licenseprovider.HasPublicKey() {
		logger.Warn("license: no public key was linked into this build, so no license verifies")
	}

	orphanpurge.Register(logger)

	if err := runtime.RunWithOptions(ctx, buildOptions()); err != nil {
		logger.Error("cms boot failed", "err", err)
		os.Exit(1)
	}
}

// buildOptions is what this build hands the engine: its identity, the
// licensing implementation it links, the capability table its plugins run
// under, and the order its security controls are listed in. The licensing
// and the capability table are decided here, before the engine reads a
// license or starts a plugin, and nothing at run time can change them.
func buildOptions() runtime.Options {
	return runtime.Options{
		Version:      version,
		Commit:       commit,
		BuildDate:    buildDate,
		ServiceName:  "lyeve-core",
		Licensing:    licenseprovider.NewProvider(),
		CapPolicy:    capPolicy,
		ControlOrder: controlOrder,
		// An install upgraded from an image that kept its schema version
		// elsewhere carries it into the engine's table on first boot.
		SchemaVersionSeed: schemacarry.Seed,
	}
}

func missingPlugins(want, got []string) []string {
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[g] = true
	}
	var missing []string
	for _, w := range want {
		if !have[w] {
			missing = append(missing, w)
		}
	}
	return missing
}
