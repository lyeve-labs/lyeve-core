package api

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
)

// SetupModeHandler is everything the engine serves while it boots without
// the settings it needs: the probes, the setup routes, and a 503 for every
// other path. It holds no database and starts no plugin, so there is nothing
// behind it to reach.
type SetupModeHandler struct {
	missing   []config.SetupSetting
	token     *SetupToken
	generated map[string]string

	mu    sync.Mutex
	shown bool
}

// NewSetupModeHandler builds the handler. generated maps the env key of each
// missing secret to a value made for this process. The status route returns
// those values once and never again, and nothing writes them anywhere.
func NewSetupModeHandler(missing []config.SetupSetting, token *SetupToken, generated map[string]string) *SetupModeHandler {
	return &SetupModeHandler{missing: missing, token: token, generated: generated}
}

// SetupRestartStep is one way to restart the engine with new settings. The
// engine cannot restart its own container, so it names the command instead.
type SetupRestartStep struct {
	Label   string `json:"label"`
	Command string `json:"command"`
}

// SetupMissingSetting is one setting the operator still has to provide.
type SetupMissingSetting struct {
	Env       string `json:"env"`
	YAML      string `json:"yaml"`
	Generated bool   `json:"generated"`
}

// SetupModeStatus is the body of GET /api/admin/setup/status.
type SetupModeStatus struct {
	Mode    string                `json:"mode"`
	Missing []SetupMissingSetting `json:"missing"`
	// SecretsIncluded is true on the one response that carries the generated
	// values. Every later response carries a placeholder in their place.
	SecretsIncluded bool               `json:"secrets_included"`
	Env             string             `json:"env"`
	YAML            string             `json:"yaml"`
	Restart         []SetupRestartStep `json:"restart"`
}

// databaseURLPlaceholder is what the env lines offer for the one value that
// cannot be generated.
const databaseURLPlaceholder = "postgres://USER:PASSWORD@HOST:5432/lyeve?sslmode=require"

// secretShownPlaceholder replaces a generated value after its one showing.
const secretShownPlaceholder = "<shown once; restart the engine for new values>"

var setupRestartSteps = []SetupRestartStep{
	{Label: "Docker Compose", Command: "docker compose up -d"},
	{Label: "Kubernetes", Command: "kubectl rollout restart deployment/<engine deployment>"},
	{Label: "systemd", Command: "sudo systemctl restart lyeve"},
	{Label: "docker run", Command: "docker rm -f <engine container>, then run it again with the new -e flags"},
}

func (h *SetupModeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every body here is either a refusal or, once, a secret.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSetupModeRequired, nil)
		return
	}
	switch r.URL.Path {
	case "/healthz", "/startup":
		// Alive: an orchestrator that restarts a live process in setup mode
		// would loop it forever and never let the operator read the screen.
		respond(w, http.StatusOK, map[string]string{"status": "ok", "mode": "setup"})
	case "/readyz":
		// Not ready: nothing but setup is served, and a load balancer or an
		// external probe must see that rather than a healthy engine.
		respond(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "mode": "setup"})
	case "/api/admin/setup":
		body := map[string]any{"setup_required": true, "mode": "setup"}
		if src := h.token.Source(); src != "" {
			body["token_source"] = src
		}
		respond(w, http.StatusOK, body)
	case "/api/admin/setup/status":
		h.status(w, r)
	default:
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSetupModeRequired, nil)
	}
}

func (h *SetupModeHandler) status(w http.ResponseWriter, r *http.Request) {
	if !h.token.Matches(r.Header.Get(SetupTokenHeader)) {
		slog.Warn("setup mode: refused a status read without a valid setup token", "remote", r.RemoteAddr)
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeSetupTokenInvalid, nil)
		return
	}

	h.mu.Lock()
	include := !h.shown && len(h.generated) > 0
	if include {
		h.shown = true
	}
	h.mu.Unlock()

	values := make(map[string]string, len(h.missing))
	out := SetupModeStatus{Mode: "setup", SecretsIncluded: include, Restart: setupRestartSteps}
	for _, s := range h.missing {
		gen, ok := h.generated[s.EnvKey]
		out.Missing = append(out.Missing, SetupMissingSetting{Env: s.EnvKey, YAML: s.YAMLPath, Generated: ok})
		switch {
		case ok && include:
			values[s.EnvKey] = gen
		case ok:
			values[s.EnvKey] = secretShownPlaceholder
		default:
			values[s.EnvKey] = databaseURLPlaceholder
		}
	}
	out.Env = setupEnvLines(h.missing, values)
	out.YAML = setupYAML(h.missing, values)
	if include {
		slog.Info("setup mode: generated secrets were read once through the setup status route", "remote", r.RemoteAddr)
	}
	respond(w, http.StatusOK, out)
}

func setupEnvLines(settings []config.SetupSetting, values map[string]string) string {
	var b strings.Builder
	for _, s := range settings {
		b.WriteString(s.EnvKey)
		b.WriteByte('=')
		b.WriteString(values[s.EnvKey])
		b.WriteByte('\n')
	}
	return b.String()
}

// setupYAML renders the settings as a lyeve.yaml fragment, grouping keys that
// share a parent. Every path here has two segments.
func setupYAML(settings []config.SetupSetting, values map[string]string) string {
	var b strings.Builder
	parent := ""
	for _, s := range settings {
		head, leaf, _ := strings.Cut(s.YAMLPath, ".")
		if head != parent {
			b.WriteString(head)
			b.WriteString(":\n")
			parent = head
		}
		b.WriteString("  ")
		b.WriteString(leaf)
		b.WriteString(": ")
		b.WriteString(yamlQuote(values[s.EnvKey]))
		b.WriteByte('\n')
	}
	return b.String()
}

// yamlQuote double-quotes a scalar, so a URL with a colon or a placeholder
// with angle brackets parses as the string it reads as.
func yamlQuote(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}
