// Package logsanitize redacts sensitive values from URLs before logging.
// Third-party services (Google Gemini, Akismet, Contentful, Telegram, Discord,
// Slack) embed API keys in query strings or paths. This package strips them so
// they never reach request logs, error messages, or debug output.
package logsanitize

import (
	"net/url"
	"regexp"
	"strings"
)

// Sensitive query parameter names whose values are replaced with ***.
var sensitiveQueryParams = map[string]bool{
	"key":          true, // Google Gemini ?key=
	"api_key":      true, // DSN-style
	"apikey":       true, // DSN-style
	"token":        true, // generic
	"access_token": true, // Contentful ?access_token=
	"auth":         true, // generic
	"secret":       true, // generic
}

// Akismet uses <api-key>.rest.akismet.com as hostname.
var akismetSubdomainRe = regexp.MustCompile(`^[^.]+\.rest\.akismet\.com$`)

// Telegram uses /bot<token>/method path.
var telegramBotPathRe = regexp.MustCompile(`/bot[^/]+/`)

// Discord webhook URL: /api/webhooks/{webhook_id}/{webhook_token}.
var discordWebhookPathRe = regexp.MustCompile(`/api/webhooks/\d+/[^/?#]+`)

// Slack webhook URL: /services/{team_id}/{service_id}/{secret}.
var slackWebhookPathRe = regexp.MustCompile(`/services/[^/]+/[^/]+/[^/?#]+`)

// RedactURL returns a copy of rawURL with the userinfo password, sensitive
// query parameters, hostname subdomains, and path segments replaced by "***".
// Returns rawURL unchanged when it cannot be parsed.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	changed := false

	// Redact the password in userinfo, which is the most ordinary way a
	// credential reaches a URL. It is Grafana Cloud's documented Loki push
	// form, and it is what a database DSN looks like. The user is kept: it
	// identifies the account rather than authenticating it, and an operator
	// reading a log needs to know which one failed. This is what net/url's own
	// Redacted does.
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "***")
			changed = true
		}
	}

	// Redact sensitive query parameters.
	q := u.Query()
	for k := range q {
		if sensitiveQueryParams[strings.ToLower(k)] {
			q.Set(k, "***")
			changed = true
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}

	// Redact Akismet-style API key in hostname subdomain.
	host := u.Hostname()
	if host == "" {
		host = u.Host
		if ci := strings.LastIndex(host, ":"); ci != -1 {
			host = host[:ci]
		}
	}
	if akismetSubdomainRe.MatchString(host) {
		parts := strings.SplitN(host, ".", 2)
		if len(parts) == 2 {
			port := u.Port()
			if port != "" {
				u.Host = "***." + parts[1] + ":" + port
			} else {
				u.Host = "***." + parts[1]
			}
			changed = true
		}
	}

	// Redact path-based tokens. Go's url.URL.String() percent-encodes * in
	// paths, so redaction runs at the string level on u.String() output.
	result := u.String()

	// url.String percent-encodes the * in userinfo exactly as it does in a
	// path, so the replacement is undone here the same way.
	result = strings.Replace(result, ":%2A%2A%2A@", ":***@", 1)

	if telegramBotPathRe.MatchString(result) {
		result = telegramBotPathRe.ReplaceAllString(result, "/bot***/")
		return result
	}
	if discordWebhookPathRe.MatchString(result) {
		// /api/webhooks/123456789/abcDEFghiJKLMnopQRSTuvWX -> /api/webhooks/***/***
		result = discordWebhookPathRe.ReplaceAllString(result, "/api/webhooks/***/***")
		return result
	}
	if slackWebhookPathRe.MatchString(result) {
		// /services/T00000000/B00000000/XXXXXXXXXXXX -> /services/***/***/***
		result = slackWebhookPathRe.ReplaceAllString(result, "/services/***/***/***")
		return result
	}
	if changed {
		return result
	}
	return rawURL
}
