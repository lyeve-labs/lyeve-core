package core

import (
	"errors"
	"net/http"
)

// StepUpChecker confirms that the person holding a signed-in session is the
// account's owner, by one proof the engine already asks for before it issues
// or rotates a token. A plugin guarding something a stolen session should
// not be enough to take asks it with the request and the answer it was given.
// The account is the one the request's session names, never one the caller
// chooses.
//
// The engine host implements it, and ScopedHost forwards it to a plugin
// granted CapStepUp and answers ErrCapDenied to every other.
// The checks count and lock out exactly as the token routes do: a wrong
// password counts against the login lockout, a wrong authenticator code
// against the MFA lockout, and a code accepted once is not accepted again
// within its window.
type StepUpChecker interface {
	// CheckStepUp checks answer as the factor named for the request's
	// account. It answers nil when the answer is accepted.
	//
	// ErrStepUpUnavailable means the account cannot give the factor at all:
	// it has no password, no authenticator is enrolled, the request
	// carries no signed-in session, or the session's account is not one of
	// the request's tenant. It is answered before the answer is read.
	// ErrStepUpRequired means the answer is empty, and is never counted.
	// ErrStepUpRefused is a wrong answer, which was counted, or a code
	// already accepted within its window, which is refused without being
	// counted. ErrStepUpLocked means the factor's lockout is reached and
	// nothing was checked. Any other error, a factor the engine does not
	// check among them, wraps ErrServiceUnavailable.
	CheckStepUp(r *http.Request, factor, answer string) error
}

// The factors CheckStepUp takes.
const (
	// StepUpPassword is the account's password.
	StepUpPassword = "password"
	// StepUpCode is a current code from the account's authenticator app.
	StepUpCode = "code"
)

// CheckStepUp's answers. See StepUpChecker.
var (
	ErrStepUpUnavailable = errors.New("step-up: the account cannot give this factor")
	ErrStepUpRequired    = errors.New("step-up: no answer was given")
	ErrStepUpRefused     = errors.New("step-up: the answer is not valid")
	ErrStepUpLocked      = errors.New("step-up: too many wrong answers")
)
