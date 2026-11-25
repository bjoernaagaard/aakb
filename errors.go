package aakb

import "errors"

// Code identifies a category of library failure.
type Code string

const (
	// CodeInvalidArgument means the request is invalid.
	CodeInvalidArgument Code = "INVALID_ARGUMENT"
	// CodeConfig means the operation needs different configuration.
	CodeConfig Code = "CONFIG"
	// CodeNotFound means the provider could not find the requested item.
	CodeNotFound Code = "NOT_FOUND"
	// CodeUpstreamBlocked means the provider denied access or issued a challenge.
	CodeUpstreamBlocked Code = "UPSTREAM_BLOCKED"
	// CodeTimeout means the operation or upstream request timed out.
	CodeTimeout Code = "REQUEST_TIMEOUT"
	// CodeCanceled means the caller canceled the operation.
	CodeCanceled Code = "CANCELED"
	// CodeUpstream means a provider failed to complete the request.
	CodeUpstream Code = "UPSTREAM"
	// CodeInternal means a local operation failed.
	CodeInternal Code = "INTERNAL"
)

// Error is a library failure with a credential-free message.
type Error struct {
	Code    Code
	Op      string
	Message string
	cause   error
}

// Error formats a library failure as "[CODE] op: message".
func (e *Error) Error() string {
	return "[" + string(e.Code) + "] " + e.Op + ": " + e.Message
}

// Unwrap exposes a context cancellation or deadline when it caused the failure.
func (e *Error) Unwrap() error { return e.cause }

// CodeOf returns the library code in err, or the empty string if there is none.
func CodeOf(err error) Code {
	if e, ok := errors.AsType[*Error](err); ok && e != nil {
		return e.Code
	}
	return ""
}

func newError(op string, code Code, message string) error {
	return &Error{Code: code, Op: op, Message: message}
}
