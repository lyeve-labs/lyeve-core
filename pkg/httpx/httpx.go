// Package httpx holds the HTTP helpers the engine and its plugins share: JSON
// responses, the localized error envelope, error-to-status mapping, request
// validation, plugin secret encryption, the 402 responses of a license gate,
// and the feature names a license may carry.
package httpx

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/go-sql-driver/mysql"

	"github.com/go-playground/validator/v10"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// JSON writes v as a JSON body with the given status code. A nil v writes only
// the status line, which is the idiomatic way to emit e.g. 204 No Content.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// Paginated writes a JSON response for paginated list endpoints. The response
// envelope includes the data items, total_count for UI pagination, and the
// limit/offset values used.
//
// Usage:
//
//	items, total, err := h.store.List(r.Context(), limit, offset)
//	if err != nil { ... }
//	httpx.Paginated(w, items, total, limit, offset)
//
// A store that returns its zero slice on an empty result hands us a typed nil,
// and `[]T(nil)` held in an `any` is not == nil: the interface carries a type,
// so an untyped-nil check alone lets it through and the envelope answers
// "data": null. Consumers that iterate the list break on that, which is the
// empty state every fresh install starts in.
func Paginated(w http.ResponseWriter, data any, totalCount, limit, offset int) {
	if isNilSlice(data) {
		data = []any{}
	}
	JSON(w, http.StatusOK, map[string]any{
		"data":        data,
		"total_count": totalCount,
		"limit":       limit,
		"offset":      offset,
	})
}

// isNilSlice reports whether data is an untyped nil or a nil slice, map, or
// pointer held in an interface. Only these marshal to null where a caller
// expects a collection.
func isNilSlice(data any) bool {
	if data == nil {
		return true
	}
	v := reflect.ValueOf(data)
	switch v.Kind() {
	case reflect.Slice, reflect.Map, reflect.Ptr:
		return v.IsNil()
	default:
		return false
	}
}

// DecodeList extracts the "data" array from a paginated JSON response written
// by [Paginated].  T is the element type of the slice you expect.
//
// Usage in tests:
//
//	items, err := httpx.DecodeList[MyType](rec.Body)
//	if err != nil { t.Fatal(err) }
func DecodeList[T any](r io.Reader) ([]T, error) {
	var resp struct {
		Data       []T `json:"data"`
		TotalCount int `json:"total_count"`
		Limit      int `json:"limit"`
		Offset     int `json:"offset"`
	}
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// ListData extracts the raw "data" field from a paginated JSON response.
// Use this when the element type is too complex for [DecodeList]'s type
// parameter (e.g. []map[string]interface{}), then json.Unmarshal the result.
//
// Usage in tests:
//
//	raw, err := httpx.ListData(rec.Body)
//	require.NoError(t, err)
//	var items []map[string]interface{}
//	require.NoError(t, json.Unmarshal(raw, &items))
func ListData(r io.Reader) (json.RawMessage, error) {
	var resp struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// Error writes a JSON {"error": msg, "code": code} body with the given status
// code. The machine-readable "code" field lets API consumers branch on error
// type without parsing the human-readable message.
func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg, "code": statusCode(status)})
}

// ErrorReq is like Error but also includes the request ID from the request
// context in the response body. Use this in admin/API handlers that have access
// to the *http.Request so that error responses carry a "request_id" field for
// log correlation.
func ErrorReq(w http.ResponseWriter, r *http.Request, status int, msg string) {
	rid := logging.RequestIDFromCtx(r.Context())
	JSON(w, status, map[string]string{
		"error":      msg,
		"code":       statusCode(status),
		"request_id": rid,
	})
}

// ErrorCode writes the localized error envelope {"error", "code",
// "request_id"}: the catalog's message for code in the caller's locale with
// params interpolated, the code itself for a client to branch on, and the
// request id to find the log line by. It is the envelope the engine's own
// handlers answer with, for a handler outside the engine that cannot reach
// the catalog. A code the catalog does not hold is its own message.
func ErrorCode(w http.ResponseWriter, r *http.Request, status int, code string, params map[string]any) {
	body := struct {
		Error     string `json:"error"`
		Code      string `json:"code,omitempty"`
		RequestID string `json:"request_id,omitempty"`
	}{
		Error:     i18n.Code(code).Error(i18n.LocaleFromCtx(r.Context()), params),
		Code:      code,
		RequestID: logging.RequestIDFromCtx(r.Context()),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The engine's own encoder leaves HTML unescaped, so this one does too
	// and a message reads the same from either.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body) // err suppressed: response write to client
}

// statusCode maps an HTTP status code to a machine-readable error code string.
func statusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusInternalServerError:
		return "internal_error"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	default:
		return fmt.Sprintf("http_%d", status)
	}
}

// StatusFor maps shared core domain sentinels to HTTP status codes. Stores
// should wrap their errors with core.ErrNotFound / core.ErrConflict (via
// %w) so handlers can call StatusFor uniformly. Unrecognized errors map to 500.
//
// An error whose kind is recognizable answers the same here as it does through
// [StoreStatusFor]: a dropped connection is 503 and a duplicate key is 409 no
// matter which mapper a handler reached for. The two differ only in what they
// assume about an error neither recognizes.
func StatusFor(err error) int {
	if status, ok := classify(err); ok {
		return status
	}
	return http.StatusInternalServerError
}

// StoreStatusFor maps store-layer errors to HTTP status codes. It classifies
// domain sentinels (ErrNotFound -> 404, ErrConflict -> 409), infrastructure
// failures (-> 503), and caller-caused database rejections (-> 409 / 422)
// exactly as [StatusFor] does.
//
// The two part on the error neither recognizes: reaching this function says the
// error came out of a store, where an unrecognized failure is far more likely to
// be the database than a bug in the engine, so the default is 503 rather than
// 500.
func StoreStatusFor(err error) int {
	if status, ok := classify(err); ok {
		return status
	}
	return http.StatusServiceUnavailable
}

// classify maps err to a status when its kind is recognizable, reporting false
// when only the calling mapper can decide what an unrecognized error means.
func classify(err error) (int, bool) {
	if err == nil {
		return http.StatusOK, true
	}

	// reqparse typed errors map to 400 Bad Request.
	var uuidErr *reqparse.InvalidUUIDError
	if errors.As(err, &uuidErr) {
		return http.StatusBadRequest, true
	}
	var missingErr *reqparse.MissingParamError
	if errors.As(err, &missingErr) {
		return http.StatusBadRequest, true
	}
	var intErr *reqparse.InvalidQueryIntError
	if errors.As(err, &intErr) {
		return http.StatusBadRequest, true
	}
	var pagErr *reqparse.InvalidPaginationError
	if errors.As(err, &pagErr) {
		return http.StatusBadRequest, true
	}

	if errors.Is(err, core.ErrNotFound) {
		return http.StatusNotFound, true
	}
	if errors.Is(err, core.ErrConflict) {
		return http.StatusConflict, true
	}
	if errors.Is(err, core.ErrForbidden) {
		return http.StatusForbidden, true
	}
	if errors.Is(err, core.ErrUnauth) {
		return http.StatusUnauthorized, true
	}
	if errors.Is(err, core.ErrServiceUnavailable) {
		return http.StatusServiceUnavailable, true
	}
	// A capability the license does not grant is refused the same way every
	// time, and 402 tells the client that the plan decides it.
	if errors.Is(err, core.ErrNotGranted) {
		return http.StatusPaymentRequired, true
	}
	// A store that fails closed on a missing tenant is reporting a request the
	// engine cannot scope, not a dependency that is down. Unclassified, it
	// would take StoreStatusFor's 503 default, which tells the caller to retry
	// a request that will fail identically every time.
	if errors.Is(err, core.ErrTenantRequired) {
		return http.StatusBadRequest, true
	}
	// Input the engine refused is the same case: it will be refused again.
	if errors.Is(err, core.ErrValidation) {
		return http.StatusBadRequest, true
	}

	// DB-specific standard library sentinels.
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return http.StatusServiceUnavailable, true
	}

	// net.ErrClosed surfaces when the underlying TCP connection is dropped
	// (common after DB restart/failover). Treat as transient infrastructure failure.
	if errors.Is(err, net.ErrClosed) {
		return http.StatusServiceUnavailable, true
	}

	// Context errors indicate timeouts or canceled requests, both transient.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return http.StatusServiceUnavailable, true
	}

	// A rejection the caller caused is not an outage. Checked before either
	// default, which otherwise tells a client to retry a request that can never
	// succeed.
	if status, ok := callerErrorStatus(err); ok {
		return status, true
	}

	// A driver reporting that the database is unreachable, out of connections,
	// shutting down, or deadlocked is an outage whichever mapper was called.
	// Left unrecognized it reaches StatusFor's 500, which reads as an engine bug
	// and tells a client not to retry something that will work in a moment.
	if status, ok := infraErrorStatus(err); ok {
		return status, true
	}

	return 0, false
}

// infraErrorStatus reports 503 for a driver error that means the database is
// unavailable rather than the request being wrong, and false for anything else.
//
// The listed codes are only those that cannot be caused by a bad request. A
// syntax error or an unknown column is deliberately absent: those are the
// engine's own bug and belong in the 500 range, where StatusFor leaves them.
func infraErrorStatus(err error) (int, bool) {
	var pgLike interface{ SQLState() string }
	if errors.As(err, &pgLike) {
		state := pgLike.SQLState()
		switch {
		case strings.HasPrefix(state, "08"), // connection exception
			strings.HasPrefix(state, "53"), // insufficient resources
			strings.HasPrefix(state, "57"), // operator intervention, shutdown
			strings.HasPrefix(state, "58"): // system error
			return http.StatusServiceUnavailable, true
		}
		switch state {
		case "40001", // serialization_failure, the caller should retry
			"40P01": // deadlock_detected
			return http.StatusServiceUnavailable, true
		}
		return 0, false
	}

	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1040, // too many connections
			1042, // cannot get hostname
			1043, // bad handshake
			1053, // server shutdown in progress
			1077, // normal shutdown
			1129, // host blocked
			1205, // lock wait timeout
			1213, // deadlock found
			2002, // cannot connect through socket
			2003, // cannot connect to server
			2006, // server has gone away
			2013: // lost connection during query
			return http.StatusServiceUnavailable, true
		}
		return 0, false
	}

	var msLike interface{ SQLErrorNumber() int32 }
	if errors.As(err, &msLike) {
		switch msLike.SQLErrorNumber() {
		case 64, // connection was successfully established but then failed
			233,   // no process on the other end of the pipe
			1205,  // chosen as the deadlock victim
			4060,  // cannot open database
			10053, // transport-level error
			10054, // existing connection forcibly closed
			10060, // network or instance-specific error
			40197, // Azure SQL is processing a request, retry
			40501, // Azure SQL busy
			40613, // Azure SQL database unavailable
			49918, // Azure SQL cannot process, not enough resources
			49919, // Azure SQL cannot process create or update request
			49920: // Azure SQL cannot process request, too many operations
			return http.StatusServiceUnavailable, true
		}
	}
	return 0, false
}

// callerErrorStatus maps a database rejection that the request caused to its
// HTTP status, reporting false for anything else.
//
// Posting a duplicate value to a unique field, or a string to a numeric one,
// is the caller's to fix, so both belong in the 4xx range. Answered with a
// 503, a client would retry forever and any monitor watching 5xx would read a
// healthy engine as down.
//
// Only rejections that cannot be caused by infrastructure are listed. A
// constraint violation and a data-type error mean the same thing whatever the
// state of the server, so classifying them cannot hide a real outage.
func callerErrorStatus(err error) (int, bool) {
	// PostgreSQL reports the ANSI SQLSTATE, read through the method pgconn
	// exposes so this package does not depend on the driver.
	var pgLike interface{ SQLState() string }
	if errors.As(err, &pgLike) {
		return sqlStateStatus(pgLike.SQLState())
	}

	// MySQL collapses every integrity violation onto SQLSTATE 23000, so the
	// vendor number is the only way to tell a duplicate key from a missing
	// foreign key. It is a struct field rather than a method, which is why this
	// is the one driver the package imports.
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1062, 1169: // duplicate entry, duplicate key on write
			return http.StatusConflict, true
		case 1048, // column cannot be null
			1264, // out of range value
			1265, // data truncated for column
			1366, // incorrect value for column
			1406, // data too long for column
			1451, // cannot delete, a child row references this
			1452, // cannot add, no matching parent row
			3819: // check constraint violated
			return http.StatusUnprocessableEntity, true
		}
		return 0, false
	}

	// SQL Server reports vendor numbers, read through the method go-mssqldb
	// exposes so this package does not depend on the driver.
	var msLike interface{ SQLErrorNumber() int32 }
	if errors.As(err, &msLike) {
		switch msLike.SQLErrorNumber() {
		case 2601, 2627, // duplicate key in index, unique constraint violated
			1505: // CREATE UNIQUE INDEX found duplicate values already in the table
			return http.StatusConflict, true
		case 245, // conversion failed
			515,  // cannot insert NULL
			547,  // foreign key or check constraint violated
			2628, // string or binary data would be truncated
			8114, // error converting data type
			8115, // arithmetic overflow on conversion
			8152: // string or binary data would be truncated (legacy)
			return http.StatusUnprocessableEntity, true
		}
	}
	return 0, false
}

// sqlStateStatus maps an ANSI SQLSTATE to its HTTP status, reporting false when
// the state is not something the caller can fix.
func sqlStateStatus(state string) (int, bool) {
	switch state {
	case "23505": // unique_violation
		return http.StatusConflict, true
	case "23502", // not_null_violation
		"23503", // foreign_key_violation
		"23514": // check_violation
		return http.StatusUnprocessableEntity, true
	}
	// Class 22 is "data exception": the value does not fit the column's type,
	// length or range. Always the request, never the server.
	if strings.HasPrefix(state, "22") {
		return http.StatusUnprocessableEntity, true
	}
	return 0, false
}

// DecodeJSON reads a request body with strict JSON decoding: unknown fields
// are rejected, and the body must contain exactly one JSON value. This prevents
// mass-assignment attacks where attackers inject extra fields that would be
// silently ignored by encoding/json's default behavior.
//
// Returns the number of bytes read. On failure, callers should respond with
// 400 Bad Request.
func DecodeJSON(r *http.Request, v any) (int64, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return 0, fmt.Errorf("read body: %w", err)
	}
	// bytes.NewReader reads the slice already in hand. Converting it to a
	// string first would copy the whole body a second time and double the peak
	// on a large import, and the decoder only reads it.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return 0, fmt.Errorf("decode JSON: %w", err)
	}
	// Reject bodies with more than one JSON value (e.g. "{}{}").
	if dec.More() {
		var discard json.RawMessage
		if err := dec.Decode(&discard); err == nil {
			return 0, fmt.Errorf("decode JSON: request body must contain a single JSON value")
		}
	}
	return int64(len(body)), nil
}

// Input validation

var (
	defaultValidate *validator.Validate
	validateOnce    sync.Once
)

func getValidate() *validator.Validate {
	validateOnce.Do(func() {
		v := validator.New()
		v.RegisterTagNameFunc(func(fld reflect.StructField) string {
			tag := fld.Tag.Get("json")
			if tag == "" || tag == "-" {
				return ""
			}
			if idx := strings.IndexByte(tag, ','); idx != -1 {
				return tag[:idx]
			}
			return tag
		})
		defaultValidate = v
	})
	return defaultValidate
}

// FieldError describes a single validation failure on a request body field.
type FieldError struct {
	Field   string `json:"field"`
	Tag     string `json:"tag"`
	Message string `json:"message"`
}

// ValidationError is returned by Validate when struct validation fails.
// Callers should respond with 400 Bad Request and the Fields slice.
type ValidationError struct {
	Fields []FieldError
}

// Error returns a summary of the first validation failure, or "validation failed"
// when no fields are present.
func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "validation failed"
	}
	return fmt.Sprintf("validation failed on field %q: %s", e.Fields[0].Field, e.Fields[0].Message)
}

// Validate runs go-playground/validator struct tags on v. Returns nil on
// success, *ValidationError on failure. Callers do:
//
//	if err := httpx.Validate(input); err != nil {
//	    var verr *httpx.ValidationError
//	    if errors.As(err, &verr) {
//	        httpx.ValidationErr(w, verr)
//	        return
//	    }
//	}
func Validate(v any) error {
	if err := getValidate().Struct(v); err != nil {
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) {
			fields := make([]FieldError, 0, len(verrs))
			for _, fe := range verrs {
				msg := fmt.Sprintf("failed on '%s'", fe.Tag())
				if fe.Param() != "" {
					msg = fmt.Sprintf("failed on '%s=%s'", fe.Tag(), fe.Param())
				}
				fields = append(fields, FieldError{
					Field:   fe.Field(),
					Tag:     fe.Tag(),
					Message: msg,
				})
			}
			return &ValidationError{Fields: fields}
		}
		return fmt.Errorf("validate: %w", err)
	}
	return nil
}

// ValidationErr writes a 400 response with structured field-level errors. The
// "error" string names the first failing field (via ValidationError.Error) so
// API consumers get actionable feedback without parsing "fields". "Fields"
// carries the full per-field detail. Field names use JSON tags (see
// getValidate's RegisterTagNameFunc), not Go identifiers.
func ValidationErr(w http.ResponseWriter, verr *ValidationError) {
	JSON(w, http.StatusBadRequest, map[string]any{
		"error":  verr.Error(),
		"fields": verr.Fields,
	})
}

// ValidateOrRespond validates v. On failure writes a 400 and returns false.
// Callers use:
//
//	if !httpx.ValidateOrRespond(w, &input) { return }
func ValidateOrRespond(w http.ResponseWriter, v any) bool {
	if err := Validate(v); err != nil {
		var verr *ValidationError
		if errors.As(err, &verr) {
			ValidationErr(w, verr)
		} else {
			// The validator's own message, written here and not by a driver, so
			// it says which field failed and nothing about the database.
			Error(w, http.StatusBadRequest, err.Error()) //nolint:raw-error-text // struct-validation text this package produces
		}
		return false
	}
	return true
}
