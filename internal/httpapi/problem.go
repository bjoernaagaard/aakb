package httpapi

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"strconv"

	"github.com/bjoernaagaard/aakb"
)

type problem struct {
	Type       string `json:"type"`
	Title      string `json:"title"`
	Status     int    `json:"status"`
	Code       string `json:"code"`
	Detail     string `json:"detail"`
	RetryAfter int    `json:"retry_after,omitzero"`
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	writeProblemResponse(w, status, code, detail, 0)
}

func writeRetryProblem(w http.ResponseWriter, retry int) {
	writeProblemResponse(w, http.StatusTooManyRequests, "RATE_LIMITED", "The request rate limit was exceeded.", retry)
}

func writeProblemResponse(w http.ResponseWriter, status int, code, detail string, retry int) {
	w.Header().Del("Location")
	writeJSON(w, status, "application/problem+json", problem{
		Type: "about:blank", Title: http.StatusText(status), Status: status,
		Code: code, Detail: detail, RetryAfter: retry,
	})
}

func writeJSON(w http.ResponseWriter, status int, mediaType string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		w.Header().Del("Location")
		status = http.StatusInternalServerError
		mediaType = "application/problem+json"
		data = []byte(`{"type":"about:blank","title":"Internal Server Error","status":500,"code":"INTERNAL","detail":"The server could not encode the response."}`)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(data) //nolint:errcheck // a write failure cannot be sent to the disconnected client
}

func (a *API) writeClientError(w http.ResponseWriter, r *http.Request, err error) {
	failure, ok := errors.AsType[*aakb.Error](err)
	if !ok || failure == nil {
		writeProblem(w, http.StatusInternalServerError, string(aakb.CodeInternal), "The server could not complete the request.")
		return
	}
	var status int
	switch failure.Code {
	case aakb.CodeInvalidArgument:
		status = http.StatusBadRequest
	case aakb.CodeNotFound:
		status = http.StatusNotFound
	case aakb.CodeConfig:
		status = http.StatusServiceUnavailable
	case aakb.CodeUpstreamBlocked, aakb.CodeUpstream:
		status = http.StatusBadGateway
	case aakb.CodeTimeout:
		status = http.StatusGatewayTimeout
	case aakb.CodeCanceled:
		a.logger.InfoContext(r.Context(), "HTTP operation canceled", "operation", failure.Op)
		return
	case aakb.CodeInternal:
		status = http.StatusInternalServerError
	default:
		writeProblem(w, http.StatusInternalServerError, string(aakb.CodeInternal), "The server could not complete the request.")
		return
	}
	writeProblem(w, status, string(failure.Code), failure.Message)
}
