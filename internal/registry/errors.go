package registry

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// BasicChallenge is the only challenge the proxy ever sends to clients.
const BasicChallenge = `Basic realm="registrygate"`

// Fixed client-facing messages for upstream failures.
const (
	messageUnauthorized       = "authentication required"
	messageCredentialRejected = "upstream rejected the proxy's credential"
	messageRealmNotAllowed    = "upstream token endpoint not allowed"
	messageUnavailable        = "upstream unavailable"
	messageMirrorUnavailable  = "mirror unavailable"
	messageNotReady           = "registrygate is not ready"
	messageManifestTooLarge   = "manifest too large"
	messageUploadUnusable     = "upstream returned an unusable upload location"
	messageSessionQuery       = "query parameter not allowed on an upload session request"
)

type errorBody struct {
	Errors []errorDetail `json:"errors"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

// WriteError answers with a registry JSON error.
func WriteError(resp http.ResponseWriter, registryErr *Error) {
	body, err := json.Marshal(errorBody{Errors: []errorDetail{{Code: registryErr.Code, Message: registryErr.Message}}})
	if err != nil {
		resp.WriteHeader(http.StatusInternalServerError)
		return
	}

	header := resp.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))

	if registryErr.Status == http.StatusUnauthorized {
		header.Set("WWW-Authenticate", BasicChallenge)
	}

	if registryErr.Status == http.StatusServiceUnavailable && header.Get("Retry-After") == "" {
		header.Set("Retry-After", "5")
	}

	resp.WriteHeader(registryErr.Status)
	_, _ = resp.Write(body)
}

func errUnauthorized() *Error {
	return &Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: messageUnauthorized}
}

func errBadGateway(message string) *Error {
	return &Error{Status: http.StatusBadGateway, Code: "UNAVAILABLE", Message: message}
}

func errNotReady() *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: messageNotReady}
}

func errManifestTooLarge() *Error {
	return &Error{Status: http.StatusBadRequest, Code: "MANIFEST_INVALID", Message: messageManifestTooLarge}
}

func errSessionQuery() *Error {
	return &Error{Status: http.StatusBadRequest, Code: "UNSUPPORTED", Message: messageSessionQuery}
}
