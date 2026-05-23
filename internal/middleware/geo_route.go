package middleware

import (
	"net/http"
)

// GeoRouteHeader is the response header indicating the data region that
// handled the request. Used by LBs and CDN edge workers for routing decisions.
const GeoRouteHeader = "X-CMS-Region"

// GeoRoute sets the X-CMS-Region response header to this instance's region, so
// a load balancer or edge worker can route on it. An empty region sets nothing.
func GeoRoute(instanceRegion string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if instanceRegion != "" {
				w.Header().Set(GeoRouteHeader, instanceRegion)
			}
			next.ServeHTTP(w, r)
		})
	}
}
