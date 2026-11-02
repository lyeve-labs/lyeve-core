package middleware

import "net/http"

// StandardMethod returns m when it is one of the nine standard methods and
// "OTHER" for anything else. net/http accepts any token as a method, so a
// value kept per method (a metric series, a latency entry) would otherwise
// grow with whatever a client sends. The result is always a constant, so it
// never shares memory with the request line.
func StandardMethod(m string) string {
	switch m {
	case http.MethodGet:
		return http.MethodGet
	case http.MethodHead:
		return http.MethodHead
	case http.MethodPost:
		return http.MethodPost
	case http.MethodPut:
		return http.MethodPut
	case http.MethodPatch:
		return http.MethodPatch
	case http.MethodDelete:
		return http.MethodDelete
	case http.MethodConnect:
		return http.MethodConnect
	case http.MethodOptions:
		return http.MethodOptions
	case http.MethodTrace:
		return http.MethodTrace
	}
	return "OTHER"
}
