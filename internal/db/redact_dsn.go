package db

import (
	"regexp"
	"strings"
)

// dsnPasswordParamRe matches a password carried as a parameter rather than in
// the userinfo: a URL query and a PostgreSQL keyword DSN both accept one.
var dsnPasswordParamRe = regexp.MustCompile(`(?i)((?:^|[?&\s])(?:password|sslpassword)\s*=\s*)('(?:[^'\\]|\\.)*'|[^&\s]*)`)

// RedactDSN returns dsn with every password in it replaced by "***", so a
// connection string can reach a log line or an error. It reads each shape the
// engine connects with: a URL, the MySQL driver form user:pass@tcp(host)/db
// with or without a scheme, and a PostgreSQL keyword DSN. The user, host and
// database stay, because they say which connection failed.
func RedactDSN(dsn string) string {
	dsn = dsnPasswordParamRe.ReplaceAllString(dsn, "${1}***")

	prefix, rest := "", dsn
	if i := strings.Index(dsn, "://"); i >= 0 {
		prefix, rest = dsn[:i+3], dsn[i+3:]
	}
	// The userinfo ends at the last @ before the query. A password may hold an
	// unescaped @, / or ?, so when nothing before the first ? is an @, the ?
	// sat inside the password and the last @ anywhere ends the userinfo.
	head := rest
	if q := strings.Index(rest, "?"); q >= 0 {
		head = rest[:q]
	}
	at := strings.LastIndex(head, "@")
	if at < 0 {
		at = strings.LastIndex(rest, "@")
	}
	if at < 0 {
		return dsn
	}
	colon := strings.Index(rest[:at], ":")
	if colon < 0 {
		return dsn
	}
	return prefix + rest[:colon+1] + "***" + rest[at:]
}
