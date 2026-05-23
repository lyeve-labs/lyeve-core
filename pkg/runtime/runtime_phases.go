package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/logstream"
	"github.com/lyeve-labs/lyeve-core/internal/tracing"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// bootLog holds the observability infrastructure initialized during boot.
type bootLog struct {
	logger        *slog.Logger
	ring          *logstream.Buffer
	enrich        *logging.EnrichHandler
	tailer        *observability.LogTailer
	traceShutdown func(context.Context) error
}

// LogRing returns the ring of recent records the slog chain feeds. The host
// hands it to whichever plugin provides core.LogRingProvider.
func (b *bootLog) LogRing() core.LogRing { return b.ring }

// EnrichHandler returns the context-aware log enrichment handler for middleware.
func (b *bootLog) EnrichHandler() *logging.EnrichHandler { return b.enrich }

// setupLogging initializes structured logging, the log-stream ring buffer,
// and the global log tailer. It then initializes OpenTelemetry tracing.
// Returns the observability state and a shutdown function for tracing.
func setupLogging(ctx context.Context, cfg *config.Config, opts Options) (*bootLog, error) {
	levelMap := observability.NewLogLevelMap(slog.LevelInfo)
	baseHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	logBuf := logstream.NewBuffer(logstream.DefaultBufferSize)
	intercepted := logstream.NewInterceptor(baseHandler, logBuf)
	enrichHandler := logging.NewEnrichHandler(intercepted, levelMap)
	logging.SetGlobalEnrichHandler(enrichHandler)
	slog.SetDefault(enrichHandler.Logger())
	logger := slog.Default()

	tailer := observability.NewLogTailer()
	observability.SetGlobalTailer(tailer)

	logger.Info("starting lyeve cms",
		"version", opts.Version,
		"commit", opts.Commit,
		"build_date", opts.BuildDate,
		"service", opts.ServiceName,
		"instance_id", cfg.InstanceID,
		"admin_addr", cfg.AdminListenAddr,
		"api_addr", cfg.APIListenAddr,
	)

	traceShutdown, err := tracing.Init(ctx, tracing.Config{
		Endpoint:            cfg.OTLPEndpoint,
		ServiceName:         opts.ServiceName,
		ServiceVersion:      opts.Version,
		InstanceID:          cfg.InstanceID,
		Insecure:            cfg.OTLPInsecure,
		SamplingRate:        cfg.TracingSamplingRate,
		TenantSamplingRates: cfg.TracingTenantSamplingRates,
	})
	if err != nil {
		logger.Error("tracing init failed", "err", err)
		return nil, fmt.Errorf("tracing init: %w", err)
	}

	return &bootLog{
		logger:        logger,
		ring:          logBuf,
		enrich:        enrichHandler,
		tailer:        tailer,
		traceShutdown: traceShutdown,
	}, nil
}
