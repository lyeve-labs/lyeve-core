package licensing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// PresentationPath is where a licensing implementation serves its
// Presentation. The engine serves nothing there itself, so on a build that
// links no implementation the path answers 404 and a console leaves out what
// it would have shown.
const PresentationPath = "/api/admin/license"

// The relations a Link names. A console places each link by its relation,
// and leaves out one whose relation it does not know.
const (
	// RelUpgrade is where an operator goes for what the license does not
	// cover.
	RelUpgrade = "upgrade"
	// RelPurchase is where a license is obtained.
	RelPurchase = "purchase"
	// RelPortal is where the license holder manages it and gets the
	// credential the install is configured with.
	RelPortal = "portal"
	// RelSupport is where an operator asks for help.
	RelSupport = "support"
	// RelDocs is where the license and how it is verified are documented.
	RelDocs = "docs"
)

// Link is one place a licensing implementation sends an operator.
type Link struct {
	// Rel is one of the Rel constants.
	Rel string `json:"rel"`
	// Label is the text a console shows for the link, as given.
	Label string `json:"label"`
	// URL is an https URL, or a path on the console that starts with a
	// single slash.
	URL string `json:"url"`
}

// Presentation is what a licensing implementation tells a console about
// itself beside the entitlement: where to send an operator, and whether the
// console can renew the license.
type Presentation struct {
	// Links is every link the implementation offers. It is never nil.
	Links []Link `json:"links"`
	// Renew says POST /api/admin/license/renew accepts
	// {"license_key": "<key>"}.
	Renew bool `json:"renew"`
}

// PresentationRoute is the route a licensing implementation serves its
// Presentation on: GET PresentationPath, to any signed-in role, asked of fn
// on every request so a renewal shows at once. A link whose URL is neither an
// https URL nor a path on the console is left out, so a console never renders
// another scheme as a link. A nil fn serves no links and no renewal.
//
// Every route under the license path keeps its bodies out of request capture
// and is closed to admin tokens, so this one is declared that way too.
func PresentationRoute(fn func(context.Context) Presentation) core.RouteDecl {
	return core.RouteDecl{
		Method:      http.MethodGet,
		Pattern:     PresentationPath,
		Group:       core.GroupAuth,
		Sensitive:   true,
		SessionOnly: true,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var p Presentation
			if fn != nil {
				p = fn(r.Context())
			}
			links := make([]Link, 0, len(p.Links))
			for _, l := range p.Links {
				if servableURL(l.URL) {
					links = append(links, l)
				}
			}
			p.Links = links
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(p) // err suppressed: response write to client
		}),
		Doc: &core.RouteDoc{
			Tag:     "Admin / License",
			Summary: "Where the licensing implementation sends an operator",
			Description: "links names each place by rel (upgrade, purchase, portal, support or docs), with the label a console shows and " +
				"an https URL or a path on the console. renew is true when POST /api/admin/license/renew accepts " +
				`{"license_key": "<key>"}. Served only by a build that links a licensing implementation.`,
			Responses: map[int]string{http.StatusOK: "OK", http.StatusUnauthorized: "Unauthorized"},
		},
	}
}

// servableURL reports whether u is an https URL with a host, or a path on the
// console: one leading slash, and no second slash or backslash after it that
// a browser would read as the start of a host.
func servableURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	if strings.HasPrefix(u, "/") {
		return !strings.HasPrefix(u, "//") && !strings.Contains(u, `\`) && parsed.Scheme == "" && parsed.Host == ""
	}
	return parsed.Scheme == "https" && parsed.Host != ""
}
