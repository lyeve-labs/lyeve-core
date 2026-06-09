package logsanitize

import (
	"testing"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "google gemini with key",
			url:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent?key=AIzaSyAbc123Def456Ghi",
			want: "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent?key=%2A%2A%2A",
		},
		{
			name: "google gemini stream with key",
			url:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:streamGenerateContent?alt=sse&key=AIzaSyAbc123",
			want: "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:streamGenerateContent?alt=sse&key=%2A%2A%2A",
		},
		{
			name: "google models list with key",
			url:  "https://generativelanguage.googleapis.com/v1beta/models?key=AIzaSyXyz",
			want: "https://generativelanguage.googleapis.com/v1beta/models?key=%2A%2A%2A",
		},
		{
			name: "contentful access_token",
			url:  "https://cdn.contentful.com/spaces/abc123/environments/master/entries?access_token=CFPAT-secret123abc&content_type=post&skip=0&limit=100",
			want: "https://cdn.contentful.com/spaces/abc123/environments/master/entries?access_token=%2A%2A%2A&content_type=post&limit=100&skip=0",
		},
		{
			name: "contentful list content types",
			url:  "https://cdn.contentful.com/spaces/abc123/environments/master/content_types?access_token=CFPAT-xyz789",
			want: "https://cdn.contentful.com/spaces/abc123/environments/master/content_types?access_token=%2A%2A%2A",
		},
		{
			name: "akismet api key in subdomain",
			url:  "https://abc123def.rest.akismet.com/1.1/comment-check",
			want: "https://***.rest.akismet.com/1.1/comment-check",
		},
		{
			name: "telegram bot token in path",
			url:  "https://api.telegram.org/bot123456:ABC-DEF1234ghIkl/sendMessage",
			want: "https://api.telegram.org/bot***/sendMessage",
		},
		{
			name: "plain url unchanged",
			url:  "https://api.telegram.org/bot***/sendMessage",
			want: "https://api.telegram.org/bot***/sendMessage",
		},
		{
			name: "no sensitive params",
			url:  "https://example.com/api/v1/posts?page=2&sort=desc",
			want: "https://example.com/api/v1/posts?page=2&sort=desc",
		},
		{
			name: "key in lowercase query param",
			url:  "https://api.example.com/v1?key=secret123&other=val",
			want: "https://api.example.com/v1?key=%2A%2A%2A&other=val",
		},
		{
			name: "KEY uppercase param redacted",
			url:  "https://api.example.com/v1?KEY=secret&foo=bar",
			want: "https://api.example.com/v1?KEY=%2A%2A%2A&foo=bar",
		},
		{
			name: "api_key param",
			url:  "https://api.example.com/v2?api_key=sk-abcdef&model=gpt4",
			want: "https://api.example.com/v2?api_key=%2A%2A%2A&model=gpt4",
		},
		{
			name: "token param",
			url:  "https://api.example.com/v3?token=ghp_secret123&scope=repo",
			want: "https://api.example.com/v3?scope=repo&token=%2A%2A%2A",
		},
		{
			name: "garbage url returned as-is",
			url:  "not-a-valid-url::://::",
			want: "not-a-valid-url::://::",
		},
		{
			name: "discord webhook with token in path",
			url:  "https://discord.com/api/webhooks/123456789012345678/abcDEFghiJKLMnopQRSTuvWXyz123",
			want: "https://discord.com/api/webhooks/***/***",
		},
		{
			name: "discord webhook with query after token",
			url:  "https://discord.com/api/webhooks/123/abc?wait=true&thread_id=456",
			want: "https://discord.com/api/webhooks/***/***?wait=true&thread_id=456",
		},
		{
			name: "slack webhook with secret in path",
			url:  "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX",
			want: "https://hooks.slack.com/services/***/***/***",
		},
		{
			name: "slack webhook unchanged when already redacted",
			url:  "https://hooks.slack.com/services/***/***/***",
			want: "https://hooks.slack.com/services/***/***/***",
		},
		{
			name: "telegram bot token with query params",
			url:  "https://api.telegram.org/bot123456:ABC-DEF/sendMessage?chat_id=999&text=hello",
			want: "https://api.telegram.org/bot***/sendMessage?chat_id=999&text=hello",
		},
		{
			name: "akismet subdomain with port",
			url:  "https://key123.rest.akismet.com:443/1.1/comment-check",
			want: "https://***.rest.akismet.com:443/1.1/comment-check",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactURL(tt.url)
			if got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

// A credential in userinfo is the most ordinary way one reaches a URL, and it
// is the only way a Loki push endpoint can carry one: that sink has no header
// auth at all. It is also the shape of a database DSN. Everything else in this
// file covers a query parameter or a path segment.
func TestRedactURL_UserinfoPassword(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"grafana cloud loki push": {
			"https://297531:glc_eyJvIjoiNTUiLCJuIjoic3RhY2sifQ==@logs-prod-3.grafana.net/loki/api/v1/push",
			"https://297531:***@logs-prod-3.grafana.net/loki/api/v1/push",
		},
		"a database dsn": {
			"postgres://cms:hunter2@db.internal:5432/lyeve",
			"postgres://cms:***@db.internal:5432/lyeve",
		},
		"user with no password is not a credential": {
			"https://reader@logs.example/loki/api/v1/push",
			"https://reader@logs.example/loki/api/v1/push",
		},
		"no userinfo at all": {
			"https://logs.example/loki/api/v1/push",
			"https://logs.example/loki/api/v1/push",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := RedactURL(c.in); got != c.want {
				t.Errorf("RedactURL(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}
