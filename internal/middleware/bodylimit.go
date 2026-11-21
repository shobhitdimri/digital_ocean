package middleware

import (
	"fmt"
	"net/http"

	"feature-flag-service/pkg/httpx"
)

// BodyLimit caps request bodies at max bytes.
//
// A request that declares a Content-Length over the limit is refused with 413
// before any of the body is read. Bodies of unknown length (chunked) or ones
// that lie about their length are cut off while being read; httpx.DecodeJSON
// turns that into the same 413 response.
func BodyLimit(max int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > max {
				httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
					fmt.Sprintf("request body must not exceed %d bytes", max))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}
