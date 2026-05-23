package runtime

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// runSetupMode serves the setup routes until ctx ends. The operator opted in
// with LYEVE_SETUP_MODE and a setting the engine cannot run without is
// missing, so nothing connects to a database and no plugin starts. The
// process exits only when it is stopped. The engine cannot restart its own
// container, so the operator sets the values and restarts it.
func runSetupMode(ctx context.Context, req *config.SetupRequiredError, logger *slog.Logger) error {
	token, err := setupModeToken(req, logger)
	if err != nil {
		return err
	}
	generated, err := generateMissingSecrets(req.Missing)
	if err != nil {
		return err
	}
	missing := make([]string, len(req.Missing))
	for i, s := range req.Missing {
		missing[i] = s.EnvKey
	}
	logger.Warn("setup mode: the engine serves only the setup routes until these are set and it restarts",
		"missing", missing, "admin_addr", req.AdminListenAddr)

	if (req.TLSCertFile == "") != (req.TLSKeyFile == "") {
		return fmt.Errorf("setup mode: TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}
	listenerTLS, err := listenerTLSConfig(req.TLSCertFile, req.TLSKeyFile)
	if err != nil {
		return fmt.Errorf("setup mode: %w", err)
	}

	handler := api.NewSetupModeHandler(req.Missing, token, generated)
	servers := []*http.Server{newBasicServer(req.AdminListenAddr, handler, listenerTLS)}
	if req.APIListenAddr != "" && req.APIListenAddr != req.AdminListenAddr {
		servers = append(servers, newBasicServer(req.APIListenAddr, handler, listenerTLS))
	}
	return serveUntilDone(ctx, "setup mode", servers, logger)
}

// setupModeToken is LYEVE_SETUP_TOKEN when set, else a token printed once.
// Setup mode has no database, so no account can exist and a token is always
// needed.
func setupModeToken(req *config.SetupRequiredError, logger *slog.Logger) (*api.SetupToken, error) {
	if req.SetupToken != "" {
		return api.NewSetupTokenFromEnv(req.SetupToken), nil
	}
	tok, plain, err := api.GenerateSetupToken()
	if err != nil {
		return nil, fmt.Errorf("setup mode: %w", err)
	}
	logger.Warn("setup: no account exists yet. Enter this setup token on the admin's first-run page. "+
		"It works until the first super admin is created. With more than one replica, set LYEVE_SETUP_TOKEN instead, "+
		"since each replica prints its own.",
		"setup_token", plain, "instance_id", req.InstanceID)
	return tok, nil
}

// generateMissingSecrets makes a value for every missing setting that takes a
// random one. The values live in this process only: the status route shows
// them once and the operator stores them where the engine will read them.
func generateMissingSecrets(missing []config.SetupSetting) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range missing {
		if !s.Secret {
			continue
		}
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, fmt.Errorf("setup mode: generate %s: %w", s.EnvKey, err)
		}
		out[s.EnvKey] = hex.EncodeToString(raw[:])
	}
	return out, nil
}

func newBasicServer(addr string, h http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		TLSConfig:         tlsConfig,
		Addr:              addr,
		Handler:           h,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// serveUntilDone serves every server until ctx ends or one fails, then shuts
// them all down. mode names the boot in the log lines and the errors.
func serveUntilDone(ctx context.Context, mode string, servers []*http.Server, logger *slog.Logger) error {
	errc := make(chan error, len(servers))
	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			return fmt.Errorf("%s: listen %s: %w", mode, srv.Addr, err)
		}
		logger.Info(mode+": listening", "addr", srv.Addr, "tls", srv.TLSConfig != nil)
		go func(srv *http.Server, ln net.Listener) {
			if srv.TLSConfig != nil {
				ln = tls.NewListener(ln, srv.TLSConfig)
			}
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(srv, ln)
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutCtx) // best effort. The process is exiting
	}
	if serveErr != nil {
		return fmt.Errorf("%s: serve: %w", mode, serveErr)
	}
	return nil
}
