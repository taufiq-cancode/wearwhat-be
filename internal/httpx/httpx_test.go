package httpx

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	const direct = "192.0.2.1"
	for _, tc := range []struct {
		name      string
		hops      int
		forwarded []string
		want      string
	}{
		{"no proxies ignores the header", 0, []string{"203.0.113.7"}, direct},
		{"one proxy reads the last entry", 1, []string{"203.0.113.7"}, "203.0.113.7"},
		{"a fake entry from the client is ignored", 1, []string{"10.0.0.1, 203.0.113.7"}, "203.0.113.7"},
		{"two proxies read second from the right", 2, []string{"203.0.113.7, 198.51.100.2"}, "203.0.113.7"},
		{"a fake entry behind two proxies is ignored", 2, []string{"10.0.0.1, 203.0.113.7, 198.51.100.2"}, "203.0.113.7"},
		{"a header sent twice counts as one list", 1, []string{"10.0.0.1", "203.0.113.7"}, "203.0.113.7"},
		{"IPv6 works", 1, []string{"2001:db8::1"}, "2001:db8::1"},
		{"too few entries falls back to the connection", 2, []string{"203.0.113.7"}, direct},
		{"no header falls back to the connection", 1, nil, direct},
		{"junk falls back to the connection", 1, []string{"not-an-address"}, direct},
		{"an empty entry falls back to the connection", 1, []string{"203.0.113.7, "}, direct},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = direct + ":1234"
			for _, value := range tc.forwarded {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := ClientIP(r, tc.hops); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The logging wrapper mustn't stop a long upload from extending its deadlines.
func TestUploadDeadlinesPassThroughLogging(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var readErr, writeErr error
	srv := httptest.NewServer(Logging(log, 0, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rc := http.NewResponseController(w)
		deadline := time.Now().Add(time.Minute)
		readErr = rc.SetReadDeadline(deadline)
		writeErr = rc.SetWriteDeadline(deadline)
	})))
	defer srv.Close()
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if readErr != nil || writeErr != nil {
		t.Errorf("read: %v, write: %v", readErr, writeErr)
	}
}
