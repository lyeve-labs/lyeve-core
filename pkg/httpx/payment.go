package httpx

import (
	"encoding/json"
	"net/http"
)

// PaymentRequired writes a 402 Payment Required response for gated plugin
// features. The body includes the plugin name, feature key, and an optional
// upgrade URL so API consumers can surface a meaningful gate message.
// upgradeURL is empty by default, so the body names no external service.
//
// The two vocabularies are not interchangeable. plugin is the plugin's Name.
// feature is the machine-readable code for this body: "feature:" and the
// feature's name, never the bare name. The name itself ("widgets-export") is
// what a license carries and what Capabilities().Features is keyed by, and
// passing one here produces a body shaped unlike every other gate's.
//
// A code is spelled one of two ways, and a client matches each exactly. The
// older codes fold each dash to an underscore: a plugin's own name, a name
// that was a plugin before it became a capability, and the multitenant
// provisioning and customization capabilities ("feature:error_tracking").
// Every other capability keeps its dashes ("feature:flow-pro",
// "feature:alerts-pro"), and so does every capability added from now on. No
// code is respelled to make the two forms one, so pass the id constant the
// feature catalog declares rather than building the string.
func PaymentRequired(w http.ResponseWriter, plugin, feature, upgradeURL string) {
	w.Header().Set("Content-Type", "application/json")
	if upgradeURL != "" {
		w.Header().Set("X-Upgrade-URL", upgradeURL)
	}
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       "payment_required",
		"plugin":      plugin,
		"feature":     feature,
		"upgrade_url": upgradeURL,
	})
}

// PaymentRequiredPublic writes a minimal 402 response without exposing plugin
// or feature names. Use this on public and auth routes, where callers may not
// be administrators. Only admin callers get the detailed feature name, through
// PaymentRequired.
func PaymentRequiredPublic(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "payment_required"})
}

// CapExceeded writes a 402 for a capacity ceiling rather than a missing
// feature: the caller may use the thing, and already holds the maximum its
// license allows.
//
// cap is the ceiling's name ("widgets.count"), limit the ceiling, and current
// what the caller already holds. The body carries the ceiling and the count so
// a console never hard-codes either: a console that held the number itself
// would keep enforcing a ceiling the license had lifted.
//
// This is not PaymentRequired with a different message. That body names a
// plugin and a feature, which a ceiling has neither of, and a caller reading
// "feature" for a cap would look for a feature name that does not exist.
func CapExceeded(w http.ResponseWriter, cap string, limit, current int, upgradeURL string) {
	w.Header().Set("Content-Type", "application/json")
	if upgradeURL != "" {
		w.Header().Set("X-Upgrade-URL", upgradeURL)
	}
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       "cap_exceeded",
		"cap":         cap,
		"limit":       limit,
		"current":     current,
		"upgrade_url": upgradeURL,
	})
}
