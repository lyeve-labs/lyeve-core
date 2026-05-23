// Package core is the stable public API for LyEve plugins.
// Plugins import this package instead of internal/* to ensure they remain
// compatible across engine versions.
//
// The Host interface is the primary entry point. In-process plugins register
// via core.RegisterPlugin, then receive a Host through the Start(ctx, host)
// lifecycle callback when the engine boots.
//
// Breaking changes to this package are semver-major changes.
// Internal packages (internal/*) must never be imported from outside the module.
package core
