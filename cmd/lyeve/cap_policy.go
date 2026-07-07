package main

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// capPolicy is the most each plugin of this build may reach through its host.
// The engine grants a plugin its entry narrowed by what the plugin declared
// through RegisterPluginWithCaps, so a plugin can drop a capability but never
// gain one. A plugin with no entry gets only what it declared, and nothing
// when it declared nothing.
//
// An entry holds a capability only when the plugin exercises it. The secret
// grant, core.CapConfigSecret, is held by the plugins that read secrets: the
// identity providers, storage, the integrations and the model clients. The
// rest cannot reach the JWT secret, the encryption key, the API key pepper or
// the keystore. No entry holds core.CapAdmin.
//
// Each grant beyond the database and routes is held for one reason:
//
//   - core.CapConfigSecret, because the plugin seals or reads a credential it
//     stores.
//   - core.CapHooks, because the plugin publishes or subscribes to an event
//     another plugin or a flow acts on. Without it the scoped host answers
//     with a bus that delivers nothing.
//   - core.CapConfigSectionsRead, because the plugin assembles a
//     configuration bundle. A section exports its owner's secrets under the
//     sealer its caller supplies, so only that plugin holds it.
var capPolicy = map[string]core.Capability{
	"ab-testing":           core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"ai":                   core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"analytics":            core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"apianalytics":         core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"apikey":               core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"audit":                core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"bulk-import":          core.CapDBWrite | core.CapRawDB | core.CapSchema | core.CapHooks | core.CapRoutes,
	"cache":                core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"captcha":              core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"cluster":              core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"content":              core.CapDBWrite | core.CapRawDB | core.CapSchema | core.CapRoutes | core.CapHooks,
	"cron":                 core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"data-export":          core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"data-residency":       core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"device-fingerprint":   core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"email":                core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"error-tracking":       core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"events":               core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes,
	"flow":                 core.CapDBRead | core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret | core.CapFlowRegistry,
	"goroutine-engine":     core.CapRoutes,
	"graphql":              core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes,
	"grpc":                 core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapConfigSecret,
	"idempotency":          core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"localization":         core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapHooks,
	"logging":              core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"magic-link":           core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"media":                core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"messagebroker":        core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"mfa":                  core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"multitenant":          core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"oauth":                core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"password-reset":       core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"permissions":          core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"pii-mask":             core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"profiler":             core.CapRoutes,
	"query-monitor":        core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"rate-limit":           core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"realtime":             core.CapDBRead | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"recommendations":      core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"request-capture":      core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"review":               core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes,
	"saml":                 core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"schema":               core.CapDBWrite | core.CapRawDB | core.CapSchema | core.CapHooks | core.CapRoutes | core.CapConfigSectionsRead,
	"scim":                 core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"search":               core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
	"storage":              core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"synthetic-monitoring": core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret,
	"telemetry":            core.CapDBWrite | core.CapRawDB | core.CapRoutes | core.CapConfigSecret | core.CapMetrics,
	"waf":                  core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"usage":                core.CapDBWrite | core.CapRawDB | core.CapRoutes,
	"webhook":              core.CapDBWrite | core.CapRawDB | core.CapHooks | core.CapRoutes | core.CapConfigSecret,
}
