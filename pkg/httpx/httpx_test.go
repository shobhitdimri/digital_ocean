package httpx_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"feature-flag-service/pkg/httpx"
	"feature-flag-service/pkg/logger"
)

type payload struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`
	Count   int    `json:"count"`
}

func newReq(body, contentType string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

func TestDecodeJSON(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int // 0 means success
		wantCode    string
		wantInMsg   string
	}{
		{name: "valid", body: `{"name":"a","count":2}`, contentType: "application/json"},
		{name: "valid with charset", body: `{"name":"a"}`, contentType: "application/json; charset=utf-8"},
		{name: "missing content type is accepted", body: `{"name":"a"}`},
		{name: "trailing newline is fine", body: "{\"name\":\"a\"}\n"},
		{name: "form content type", body: `{"name":"a"}`, contentType: "application/x-www-form-urlencoded", wantStatus: 415, wantCode: "unsupported_media_type"},
		{name: "garbage content type", body: `{}`, contentType: ";;;", wantStatus: 415, wantCode: "unsupported_media_type"},
		{name: "empty body", body: ``, wantStatus: 400, wantCode: "invalid_request", wantInMsg: "empty"},
		{name: "whitespace only", body: "  \n", wantStatus: 400, wantCode: "invalid_request", wantInMsg: "empty"},
		{name: "malformed", body: `{"name":`, wantStatus: 400, wantCode: "invalid_request", wantInMsg: "not complete"},
		{name: "syntax error", body: `{name: 1}`, wantStatus: 400, wantCode: "invalid_request", wantInMsg: "not valid JSON"},
		{name: "unknown field", body: `{"name":"a","enabeld":true}`, wantStatus: 400, wantCode: "invalid_request", wantInMsg: `unknown field "enabeld"`},
		{name: "wrong type", body: `{"count":"many"}`, wantStatus: 400, wantCode: "invalid_request", wantInMsg: `"count"`},
		{name: "not an object", body: `[1,2]`, wantStatus: 400, wantCode: "invalid_request", wantInMsg: "JSON object"},
		{name: "null literal is accepted as no-op object", body: `null`},
		{name: "two values", body: `{"name":"a"}{"name":"b"}`, wantStatus: 400, wantCode: "invalid_request", wantInMsg: "single JSON value"},
		{name: "trailing garbage", body: `{"name":"a"} x`, wantStatus: 400, wantCode: "invalid_request", wantInMsg: "single JSON value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p payload
			err := httpx.DecodeJSON(newReq(tt.body, tt.contentType), &p)
			if tt.wantStatus == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var re *httpx.RequestError
			if !errors.As(err, &re) {
				t.Fatalf("err = %v (%T), want *RequestError", err, err)
			}
			if re.Status != tt.wantStatus || re.Code != tt.wantCode {
				t.Fatalf("got %d/%s, want %d/%s (%s)", re.Status, re.Code, tt.wantStatus, tt.wantCode, re.Message)
			}
			if !strings.Contains(re.Message, tt.wantInMsg) {
				t.Fatalf("message %q lacks %q", re.Message, tt.wantInMsg)
			}
		})
	}
}

func TestDecodeJSONFillsFields(t *testing.T) {
	var p payload
	if err := httpx.DecodeJSON(newReq(`{"name":"flag","enabled":false,"count":7}`, "application/json"), &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "flag" || p.Enabled == nil || *p.Enabled || p.Count != 7 {
		t.Fatalf("decoded %+v", p)
	}
}

func TestDecodeJSONBodyTooLarge(t *testing.T) {
	for name, body := range map[string]string{
		"first value":    `{"name":"` + strings.Repeat("a", 200) + `"}`,
		"trailing bytes": `{"name":"a"}` + strings.Repeat(" ", 200) + `x`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := newReq(body, "application/json")
			r.Body = http.MaxBytesReader(rec, r.Body, 64)

			var p payload
			var re *httpx.RequestError
			if err := httpx.DecodeJSON(r, &p); !errors.As(err, &re) {
				t.Fatalf("err = %v", err)
			}
			if re.Status != http.StatusRequestEntityTooLarge || re.Code != "body_too_large" || !strings.Contains(re.Message, "64") {
				t.Fatalf("got %+v", re)
			}
		})
	}
}

func TestRequestErrorImplementsError(t *testing.T) {
	var err error = &httpx.RequestError{Status: 400, Code: "c", Message: "boom"}
	if err.Error() != "boom" {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, http.StatusCreated, map[string]int{"a": 1})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if strings.TrimSpace(rec.Body.String()) != `{"a":1}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestWriteJSONEncodingFailureIsACleanInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (not a half-written 200)", rec.Code)
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "internal" {
		t.Fatalf("body = %q (%v)", rec.Body.String(), err)
	}
}

func TestWriteErrorShapeAndRequestID(t *testing.T) {
	base := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := logger.WithRequest(httptest.NewRequest("GET", "/", nil).Context(), base, "req-42", "alice")
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

	rec := httptest.NewRecorder()
	httpx.WriteError(rec, r, http.StatusNotFound, "not_found", "flag \"x\" not found")

	var got map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"code": "not_found", "message": `flag "x" not found`, "request_id": "req-42"}
	if len(got["error"]) != len(want) {
		t.Fatalf("error object = %v", got["error"])
	}
	for k, v := range want {
		if got["error"][k] != v {
			t.Errorf("%s = %q, want %q", k, got["error"][k], v)
		}
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestWriteErrorOmitsRequestIDWhenUnknown(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.WriteError(rec, httptest.NewRequest("GET", "/", nil), 400, "invalid_request", "bad")
	if strings.Contains(rec.Body.String(), "request_id") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
