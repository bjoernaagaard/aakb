package aakb

import (
	"context"
	"errors"

	"github.com/bjoernaagaard/aakb/internal/httpx"
)

// classify maps context errors and structured httpx.ProviderErrors, which
// carry credential-free safe messages, into library Errors.
func classify(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	return classifyError(op, err)
}

func classifyError(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return &Error{Code: CodeCanceled, Op: op, Message: "operation canceled", cause: context.Canceled}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Op: op, Message: "operation timed out", cause: context.DeadlineExceeded}
	}

	if libErr, ok := errors.AsType[*Error](err); ok && libErr != nil {
		return libErr
	}

	if pe, ok := errors.AsType[*httpx.ProviderError](err); ok && pe != nil {
		return classifyProviderError(op, pe)
	}

	return newError(op, CodeInternal, "unexpected provider failure")
}

func classifyProviderError(op string, pe *httpx.ProviderError) error {
	var code Code
	var fallback string
	switch pe.Code {
	case httpx.CodeInvalidArgument:
		code, fallback = CodeInvalidArgument, "provider rejected the request parameters"
	case httpx.CodeConfig:
		code, fallback = CodeConfig, "provider access requires valid configuration and credentials"
	case httpx.CodeNotFound:
		code, fallback = CodeNotFound, "provider could not find the requested item"
	case httpx.CodeUpstreamBlocked:
		code, fallback = CodeUpstreamBlocked, "provider blocked access to the request"
	case httpx.CodeTimeout:
		code, fallback = CodeTimeout, "provider request timed out"
	case httpx.CodeCanceled:
		return &Error{Code: CodeCanceled, Op: op, Message: "operation canceled", cause: context.Canceled}
	case httpx.CodeUpstream:
		code, fallback = CodeUpstream, "provider could not complete the request"
	case httpx.CodeInternal:
		code, fallback = CodeInternal, "unexpected provider failure"
	default:
		code, fallback = CodeInternal, "unexpected provider failure"
	}
	msg := pe.SafeMessage
	if msg == "" {
		msg = fallback
	}
	return newError(op, code, msg)
}
