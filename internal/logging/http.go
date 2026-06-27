package logging

import "net/http"

// ResponseRecorder wraps an http.ResponseWriter to capture the response status
// code and the number of body bytes written, for access logging. It forwards
// Flush so streaming responses are unaffected, and exposes the underlying
// writer via Unwrap for any further interface probing.
type ResponseRecorder struct {
	http.ResponseWriter
	Status int
	Bytes  int64
}

// NewResponseRecorder wraps w. Status defaults to 200 if the handler writes a
// body without an explicit WriteHeader.
func NewResponseRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w}
}

func (r *ResponseRecorder) WriteHeader(code int) {
	if r.Status == 0 {
		r.Status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *ResponseRecorder) Write(b []byte) (int, error) {
	if r.Status == 0 {
		r.Status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.Bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports http.Flusher; it is
// a no-op otherwise so callers can always rely on the method existing.
func (r *ResponseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the wrapped writer (used by http.ResponseController and tests).
func (r *ResponseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// StatusOrOK returns the recorded status, defaulting to 200 when the handler
// completed without ever writing a header or body.
func (r *ResponseRecorder) StatusOrOK() int {
	if r.Status == 0 {
		return http.StatusOK
	}
	return r.Status
}
