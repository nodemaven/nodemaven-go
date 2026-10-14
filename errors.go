package nodemaven

import "errors"

// The kinds of error this package returns. Test for them with errors.Is:
//
//	if errors.Is(err, nodemaven.ErrParam) { ... }
//
// The names line up with the other NodeMaven SDKs: param, credentials,
// provider, check, api, auth, not_found, rate_limit. ErrAuth, ErrNotFound and
// ErrRateLimit are kinds of ErrAPI, so errors.Is(err, ErrAPI) is true for all
// four.
var (
	// ErrParam is input the gateway would silently ignore, misreport, or hang on.
	ErrParam = errors.New("nodemaven: parameter")
	// ErrCredentials is a missing or malformed login, password, host or port,
	// or a missing API key.
	ErrCredentials = errors.New("nodemaven: credentials")
	// ErrProvider is a provider definition that does not describe a gateway.
	ErrProvider = errors.New("nodemaven: provider")
	// ErrCheck is a CONNECT that produced no answer at all. A refused tunnel is
	// not this error: it comes back as a CheckResult carrying the status.
	ErrCheck = errors.New("nodemaven: check")
	// ErrAPI is an account API error: an error status, a body that is not
	// JSON, or no response at all.
	ErrAPI = errors.New("nodemaven: api")
	// ErrAuth is a 401 or 403 from the account API.
	ErrAuth = errors.New("nodemaven: auth")
	// ErrNotFound is a 404 from the account API, where a 404 does not end a
	// paged walk.
	ErrNotFound = errors.New("nodemaven: not found")
	// ErrRateLimit is a 429 from the account API. Nothing here retries.
	ErrRateLimit = errors.New("nodemaven: rate limit")
)

// Error is every error this package returns. Kind is one of the Err values
// above; Status is the HTTP status behind an account API error, or 0 when no
// status came back.
//
// It carries no response body: two of the account API's endpoints return
// live proxy passwords, and an error's text goes wherever the caller's logs go.
type Error struct {
	Kind    error
	Status  int
	Message string
	cause   error
}

func (e *Error) Error() string { return e.Message }

// Unwrap returns the failure underneath, when there is one: a network error,
// a timeout, or the context being cancelled.
func (e *Error) Unwrap() error { return e.cause }

// Is reports whether e is of the given kind. The three account API kinds are
// also ErrAPI.
func (e *Error) Is(target error) bool {
	if target == e.Kind {
		return true
	}
	if target == ErrAPI {
		return e.Kind == ErrAuth || e.Kind == ErrNotFound || e.Kind == ErrRateLimit
	}
	return false
}

func newError(kind error, message string) *Error {
	return &Error{Kind: kind, Message: message}
}

func apiError(status int, message string) *Error {
	return &Error{Kind: ErrAPI, Status: status, Message: message}
}

const redacted = "***"
