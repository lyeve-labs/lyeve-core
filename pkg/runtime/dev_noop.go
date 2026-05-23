//go:build !dev

package runtime

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// wireHotReload is a no-op in production builds.
//
// The signature is spelled out rather than taking any, so that it matches
// dev_wire.go exactly. The two are the same function under opposite build tags.
// If this one accepted any, the dev variant could drift to a type that no
// longer exists, and -tags dev would stop compiling without the default build
// noticing.
func wireHotReload(_ context.Context, _ *plugin.Activator) error { return nil }
