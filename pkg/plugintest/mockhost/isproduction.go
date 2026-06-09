package mockhost

import (
	"os"
	"strings"
)

// isProductionEnv answers is_production the way the engine does.
//
// config.Config.IsProduction lowercases APP_ENV and compares it to
// "production", and APP_ENV itself defaults to "production" when unset. It does
// not trim: " production" is not production to the engine, and a harness that
// trimmed would tell a plugin test it was on the hardened path while the engine
// took the other one, the divergence running in the direction that hides a
// leak rather than one that shows it.
//
// Duplicated rather than imported: mockhost is a leaf and importing plugintest
// would point the dependency the wrong way.
func isProductionEnv() bool {
	v, ok := os.LookupEnv("APP_ENV")
	if !ok || v == "" {
		return true
	}
	return strings.ToLower(v) == "production"
}
