package core

import "context"

// Reloadable is an optional interface a plugin implements when it caches
// configuration rather than reading it per request.
//
// Configuration set through the admin UI is installed on the running engine as
// soon as it is saved, so a plugin that calls host.Config() or core.Secret() on
// each request already serves the new value. A plugin that read its settings
// once in Start holds the old ones until the process restarts, which is the
// shape of bug that leaves an operator staring at a saved API key that nothing
// is using.
//
// Implement this to rebuild whatever Start derived from configuration. It is
// called after every save, on a background context, and may be called
// concurrently with requests the plugin is already serving: guard the state it
// replaces.
//
// Returning an error leaves the plugin running with the state it had. The
// engine logs the failure and does not stop or restart the plugin, because a
// bad value in one plugin's configuration must not take the process down.
type Reloadable interface {
	Plugin

	// ReloadConfig rebuilds configuration-derived state. It must be safe to
	// call repeatedly, and safe to call when nothing has actually changed.
	ReloadConfig(ctx context.Context) error
}
