package main

// controlOrder is the order this build's security controls report lists its
// controls in, which is the order the console shows them in. The plugins that
// supply them report in plugin name order. These are control names, which the
// console reads, and not the names of the plugins that report them.
var controlOrder = []string{
	"pii-mask",
	"data-residency",
	"quota/rate-limit",
	"quota",
	"waf",
	"mfa",
	"session-anomaly",
}
