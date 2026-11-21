package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// RequestError is a client mistake found while reading a request. It carries
// the status and code to answer with.
type RequestError struct {
	Status  int
	Code    string
	Message string
}

func (e *RequestError) Error() string { return e.Message }

func badRequest(format string, args ...any) *RequestError {
	return &RequestError{Status: http.StatusBadRequest, Code: "invalid_request", Message: fmt.Sprintf(format, args...)}
}

// DecodeJSON reads exactly one JSON value from the request body into dst. It is
// strict on purpose:
//
//   - a Content-Type other than application/json is rejected (415); a missing
//     Content-Type is accepted
//   - unknown fields are rejected, so a typo such as "enabeld" fails loudly
//     instead of being silently ignored
//   - an empty body, malformed JSON, wrong field types and trailing data are
//     all rejected (400)
//   - a body over the size limit set by the BodyLimit middleware is rejected
//     (413)
//
// Every failure is a *RequestError.
func DecodeJSON(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "application/json" {
			return &RequestError{
				Status:  http.StatusUnsupportedMediaType,
				Code:    "unsupported_media_type",
				Message: "Content-Type must be application/json",
			}
		}
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return classify(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return classify(err)
			}
		}
		return badRequest("request body must contain a single JSON value")
	}
	return nil
}

func classify(err error) *RequestError {
	var (
		tooBig  *http.MaxBytesError
		syntax  *json.SyntaxError
		typeErr *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &tooBig):
		return &RequestError{
			Status:  http.StatusRequestEntityTooLarge,
			Code:    "body_too_large",
			Message: fmt.Sprintf("request body must not exceed %d bytes", tooBig.Limit),
		}
	case errors.Is(err, io.EOF):
		return badRequest("request body must not be empty")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("request body is not complete JSON")
	case errors.As(err, &syntax):
		return badRequest("request body is not valid JSON (at byte %d)", syntax.Offset)
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return badRequest("field %q must be of type %s", typeErr.Field, typeErr.Type)
		}
		return badRequest("request body must be a JSON object")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return badRequest("%s", strings.TrimPrefix(err.Error(), "json: "))
	default:
		return badRequest("request body is not valid")
	}
}
