package logsanitize_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/logsanitize"
)

const botToken = "123456:AAHfakefakefakefakefakefakefakefakeX"

// TestError_TransportFailureDropsTheCredential is the case RedactURL cannot
// handle on its own.
//
// A transport failure arrives as a *url.Error whose message is the whole
// request URL. Wrapping it with a redacted copy of that same URL prints the
// redacted one and then the raw one immediately after, so the credential ends
// up in the log either way.
func TestError_TransportFailureDropsTheCredential(t *testing.T) {
	raw := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + botToken + "/sendMessage",
		Err: context.DeadlineExceeded,
	}

	// The unsafe form: a redacted copy of the URL, then the raw error that
	// repeats it.
	unsafe := fmt.Errorf("telegram: http post (%s): %w",
		logsanitize.RedactURL(raw.URL), raw)
	if !strings.Contains(unsafe.Error(), botToken) {
		t.Fatal("the unsafe form does not leak, so this test asserts nothing")
	}

	safe := fmt.Errorf("telegram: http post: %w", logsanitize.Error(raw))
	if strings.Contains(safe.Error(), botToken) {
		t.Errorf("bot token survived redaction: %s", safe.Error())
	}
	if !strings.Contains(safe.Error(), "bot***") {
		t.Errorf("redaction marker missing, so the URL was dropped rather than redacted: %s", safe.Error())
	}
}

// TestError_KeepsTheCauseAnswerable proves redaction does not cost callers the
// ability to tell a timeout from a refused connection.
func TestError_KeepsTheCauseAnswerable(t *testing.T) {
	raw := &url.Error{
		Op:  "Get",
		URL: "https://api.telegram.org/bot" + botToken + "/getMe",
		Err: context.DeadlineExceeded,
	}
	safe := logsanitize.Error(raw)
	if !errors.Is(safe, context.DeadlineExceeded) {
		t.Error("errors.Is does not reach the transport cause")
	}
	// The chain must not lead back to the unredacted URL.
	for e := safe; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), botToken) {
			t.Errorf("an error in the chain still carries the token: %s", e.Error())
		}
	}
}

// TestError_RedactsAUrlFormattedIntoAMessage covers the case where the URL is
// not a value anyone holds, only text inside a longer string.
func TestError_RedactsAUrlFormattedIntoAMessage(t *testing.T) {
	inner := fmt.Errorf("giving up after 3 attempts to https://hooks.slack.com/services/T00/B00/sekret")
	safe := logsanitize.Error(inner)
	if strings.Contains(safe.Error(), "sekret") {
		t.Errorf("slack webhook secret survived: %s", safe.Error())
	}
}

// TestError_LeavesAnUnrelatedErrorAlone keeps the helper from rewriting
// messages it has no business touching.
func TestError_LeavesAnUnrelatedErrorAlone(t *testing.T) {
	inner := errors.New("scan providers: no rows in result set")
	if got := logsanitize.Error(inner); !errors.Is(got, inner) {
		t.Errorf("unrelated error was rewritten to %q", got.Error())
	}
	if logsanitize.Error(nil) != nil {
		t.Error("nil must stay nil")
	}
}

// TestRedactText_HandlesEachServiceInThePackage checks the text-level pass
// against the same shapes RedactURL knows, since that is what an error message
// actually contains.
func TestRedactText_HandlesEachServiceInThePackage(t *testing.T) {
	cases := map[string]string{
		"https://api.telegram.org/bot" + botToken + "/sendMessage":           botToken,
		"https://hooks.slack.com/services/T000/B000/zzzsecret":               "zzzsecret",
		"https://discord.com/api/webhooks/123456/tokentokentoken":            "tokentokentoken",
		"https://generativelanguage.googleapis.com/v1/models?key=AIzaSecret": "AIzaSecret",
	}
	for raw, secret := range cases {
		got := logsanitize.RedactText("call failed: " + raw + " timed out")
		if strings.Contains(got, secret) {
			t.Errorf("secret survived in %q -> %q", raw, got)
		}
	}
}
