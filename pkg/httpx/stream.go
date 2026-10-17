package httpx

import (
	"errors"
	"net/http"
	"time"
)

// StreamDeadline keeps a long-lived response's write deadline ahead of its
// next write.
//
// net/http sets a response's write deadline once, when it reads the request,
// and fails the first write after it, which ends the request and cancels its
// context. A stream that has to outlive the server's write timeout calls Hold
// before every write, and Hold moves the deadline ahead by a fixed distance.
// A stream with a heartbeat keeps that distance at two heartbeats: HTTP/2
// resets a stream when its deadline passes even with no write pending, so one
// heartbeat would leave no margin. A client that stops reading still loses its
// connection once a write has waited that long.
//
// A StreamDeadline is used from the goroutine that writes the response.
type StreamDeadline struct {
	rc          *http.ResponseController
	ahead       time.Duration
	unsupported func(error)
	// fixed is set once the deadline cannot be moved, so Hold stops asking.
	fixed bool
}

// NewStreamDeadline returns the deadline keeper for the stream written to w,
// moving the deadline the distance ahead on each Hold. unsupported is called
// once, on the first Hold, when the writer chain cannot move the deadline,
// which is what a wrapper that does not implement Unwrap causes. It may be
// nil.
func NewStreamDeadline(w http.ResponseWriter, ahead time.Duration, unsupported func(error)) *StreamDeadline {
	return &StreamDeadline{
		rc:          http.NewResponseController(w),
		ahead:       ahead,
		unsupported: unsupported,
	}
}

// Hold moves the write deadline ahead and reports whether the stream should
// go on writing. A chain that cannot move the deadline is reported and the
// stream goes on, ending at the server's deadline as it did before it held
// its own. Any other refusal means the connection has closed, and Hold
// reports false.
func (d *StreamDeadline) Hold() bool {
	if d.fixed {
		return true
	}
	err := d.rc.SetWriteDeadline(time.Now().Add(d.ahead))
	if err == nil {
		return true
	}
	d.fixed = true
	if errors.Is(err, http.ErrNotSupported) {
		if d.unsupported != nil {
			d.unsupported(err)
		}
		return true
	}
	return false
}
