package logsanitize

import (
	"errors"
	"net/url"
	"regexp"
)

// urlInTextRe finds an absolute http(s) URL inside arbitrary text. The stop
// set is what ordinarily terminates a URL in an error message: whitespace, a
// closing quote or bracket, and a comma.
var urlInTextRe = regexp.MustCompile(`https?://[^\s"'` + "`" + `<>\[\](),]+`)

// RedactText returns s with every http(s) URL in it passed through RedactURL.
//
// RedactURL only helps when the caller holds the URL as its own string. Once a
// URL has been formatted into a longer message there is no separate value left
// to redact, which is the usual case for an error: the message is all there is.
func RedactText(s string) string {
	return urlInTextRe.ReplaceAllStringFunc(s, RedactURL)
}

// redactedError carries a message with no credential in it, and unwraps to the
// cause rather than to the original error, so nothing downstream can format
// the unredacted text back into a log line.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// Error returns err with any URL in its message redacted.
//
// Wrapping is not enough on its own. A transport failure arrives as a
// *url.Error whose message is the whole request URL, so
//
//	fmt.Errorf("send (%s): %w", RedactURL(apiURL), err)
//
// prints the redacted URL and then the raw one, credential included, straight
// after it. Telegram puts the bot token in the path and Slack and Discord put
// their webhook secrets there, so the URL is the credential for those services
// and any log line carrying it is a leak.
//
// The returned error unwraps to the transport cause rather than to the
// *url.Error, so errors.Is still answers for context.DeadlineExceeded and the
// rest, and the URL is gone from the chain entirely.
func Error(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		msg := ue.Op + ` "` + RedactURL(ue.URL) + `"`
		if ue.Err != nil {
			msg += ": " + RedactText(ue.Err.Error())
		}
		return &redactedError{msg: msg, cause: ue.Err}
	}
	if redacted := RedactText(err.Error()); redacted != err.Error() {
		return &redactedError{msg: redacted, cause: errors.Unwrap(err)}
	}
	return err
}
