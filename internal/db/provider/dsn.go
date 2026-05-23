package provider

import (
	"net/url"
	"strconv"
	"strings"
)

// DSNInfo holds the parsed components of a database connection string.
// All fields are optional: engines fill what they can from their DSN format.
type DSNInfo struct {
	Engine   string            // "postgres", "mysql", "mssql"
	Host     string            // hostname or IP
	Port     int               // port number, 0 if not specified
	DBName   string            // database name
	User     string            // username
	Password string            // password (redacted in String())
	Params   map[string]string // additional query parameters
	Raw      string            // original DSN string
}

// String returns the DSN parameters as a debug-safe string (password redacted).
func (d DSNInfo) String() string {
	pass := ""
	if d.Password != "" {
		pass = "***"
	}
	return strings.Join([]string{
		"engine=" + d.Engine,
		"host=" + d.Host,
		"port=" + strconv.Itoa(d.Port),
		"dbname=" + d.DBName,
		"user=" + d.User,
		"password=" + pass,
		"params=" + strings.Join(mapPairs(d.Params), ","),
	}, " ")
}

func mapPairs(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// DSNParser extracts structured information from an engine-specific DSN string.
type DSNParser interface {
	// Parse extracts DSNInfo from the raw DSN string.
	Parse(dsn string) DSNInfo
}

// URL-style parser (postgres).

type urlDSNParser struct{ engine string }

func (p urlDSNParser) Parse(dsn string) DSNInfo {
	info := DSNInfo{Engine: p.engine, Raw: dsn}
	u, err := url.Parse(dsn)
	if err != nil {
		return info
	}
	info.Host = u.Hostname()
	if port := u.Port(); port != "" {
		info.Port, _ = strconv.Atoi(port) // err suppressed: defaults to 0, validated downstream
	}
	if u.Path != "" {
		info.DBName = strings.TrimPrefix(u.Path, "/")
	}
	if u.User != nil {
		info.User = u.User.Username()
		info.Password, _ = u.User.Password() // err suppressed: empty string if no password set
	}
	info.Params = make(map[string]string)
	for k, v := range u.Query() {
		if len(v) > 0 {
			info.Params[k] = v[0]
		}
	}
	return info
}

// MySQL DSN parser (user:pass@tcp(host:port)/dbname).

type mysqlDSNParser struct{}

func (p mysqlDSNParser) Parse(dsn string) DSNInfo {
	info := DSNInfo{Engine: "mysql", Raw: dsn}
	// Strip params
	params := map[string]string{}
	if idx := strings.IndexByte(dsn, '?'); idx >= 0 {
		q := dsn[idx+1:]
		for _, kv := range strings.Split(q, "&") {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) == 2 {
				params[parts[0]] = parts[1]
			}
		}
		dsn = dsn[:idx]
	}
	info.Params = params

	// user:pass@tcp(host:port)/dbname
	if idx := strings.Index(dsn, "@tcp("); idx >= 0 {
		userPart := dsn[:idx]
		if ci := strings.IndexByte(userPart, ':'); ci >= 0 {
			info.User = userPart[:ci]
			info.Password = userPart[ci+1:]
		} else {
			info.User = userPart
		}
		rest := dsn[idx+5:] // after "@tcp("
		if ci := strings.IndexByte(rest, ')'); ci >= 0 {
			hostPort := rest[:ci]
			if hpi := strings.LastIndex(hostPort, ":"); hpi >= 0 {
				info.Host = hostPort[:hpi]
				info.Port, _ = strconv.Atoi(hostPort[hpi+1:]) // err suppressed: defaults to 0, validated downstream
			} else {
				info.Host = hostPort
			}
			rest = rest[ci+1:]
		}
		info.DBName = strings.TrimPrefix(rest, "/")
	}
	return info
}

// MSSQL DSN parser (sqlserver://user:pass@host:port?database=dbname).

type mssqlDSNParser struct{}

func (p mssqlDSNParser) Parse(dsn string) DSNInfo {
	info := DSNInfo{Engine: "mssql", Raw: dsn}
	u, err := url.Parse(dsn)
	if err != nil {
		return info
	}
	info.Host = u.Hostname()
	if port := u.Port(); port != "" {
		info.Port, _ = strconv.Atoi(port) // err suppressed: defaults to 0, validated downstream
	}
	if u.User != nil {
		info.User = u.User.Username()
		info.Password, _ = u.User.Password() // err suppressed: empty string if no password set
	}
	info.Params = make(map[string]string)
	for k, v := range u.Query() {
		if len(v) > 0 {
			info.Params[k] = v[0]
		}
	}
	info.DBName = info.Params["database"]
	return info
}
