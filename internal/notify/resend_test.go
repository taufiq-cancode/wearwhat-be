package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

type resendCall struct {
	auth, idempotency, contentType string
	body                           map[string]any
}

// fakeResend answers each attempt with the next status in the list, then 200 after that.
func fakeResend(t *testing.T, statuses ...int) (*Resend, func() []resendCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []resendCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emails" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		call := resendCall{auth: r.Header.Get("Authorization"), idempotency: r.Header.Get("Idempotency-Key"), contentType: r.Header.Get("Content-Type")}
		if err := json.NewDecoder(r.Body).Decode(&call.body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		calls = append(calls, call)
		n := len(calls)
		mu.Unlock()
		if n <= len(statuses) {
			w.WriteHeader(statuses[n-1])
			_, _ = io.WriteString(w, `{"name":"some_error","message":"from Resend"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`)
	}))
	t.Cleanup(srv.Close)
	mailer := &Resend{Key: "re_test", From: "wearwhat <hello@wearwhat.test>", Base: srv.URL, Backoff: time.Millisecond}
	return mailer, func() []resendCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func TestResendSendsThePlainTextMessage(t *testing.T) {
	mailer, calls := fakeResend(t)
	if err := mailer.Send(context.Background(), "buyer@test.dev", "Your code", "It’s 123456"); err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("%d calls", len(got))
	}
	if got[0].auth != "Bearer re_test" || got[0].contentType != "application/json" {
		t.Errorf("headers: %+v", got[0])
	}
	if got[0].idempotency == "" {
		t.Error("no idempotency key")
	}
	want := map[string]any{"from": "wearwhat <hello@wearwhat.test>", "to": []any{"buyer@test.dev"}, "subject": "Your code", "text": "It’s 123456"}
	if !reflect.DeepEqual(got[0].body, want) {
		t.Errorf("body:\n got %v\nwant %v", got[0].body, want)
	}
}

func TestResendRetriesTemporaryFailuresWithTheSameKey(t *testing.T) {
	for name, statuses := range map[string][]int{
		"rate limited": {http.StatusTooManyRequests},
		"server error": {http.StatusInternalServerError, http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			mailer, calls := fakeResend(t, statuses...)
			if err := mailer.Send(context.Background(), "a@test.dev", "s", "b"); err != nil {
				t.Fatal(err)
			}
			got := calls()
			if len(got) != len(statuses)+1 {
				t.Fatalf("%d calls", len(got))
			}
			for _, call := range got {
				if call.idempotency != got[0].idempotency {
					t.Error("a retry used a new idempotency key, so it could deliver twice")
				}
			}
		})
	}
}

func TestResendGivesUpInTheRightPlaces(t *testing.T) {
	t.Run("a rejected message isn't retried", func(t *testing.T) {
		for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity} {
			mailer, calls := fakeResend(t, status)
			err := mailer.Send(context.Background(), "a@test.dev", "s", "b")
			if err == nil {
				t.Errorf("%d: no error", status)
			}
			if n := len(calls()); n != 1 {
				t.Errorf("%d: %d attempts", status, n)
			}
		}
	})
	t.Run("retries stop after three attempts", func(t *testing.T) {
		mailer, calls := fakeResend(t, 500, 500, 500, 500, 500)
		if err := mailer.Send(context.Background(), "a@test.dev", "s", "b"); err == nil {
			t.Error("no error")
		}
		if n := len(calls()); n != resendAttempts {
			t.Errorf("%d attempts", n)
		}
	})
	t.Run("the error explains itself, without the recipient", func(t *testing.T) {
		mailer, _ := fakeResend(t, http.StatusForbidden)
		err := mailer.Send(context.Background(), "private.person@test.dev", "s", "b")
		if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "from Resend") {
			t.Errorf("error: %v", err)
		}
		if err != nil && strings.Contains(err.Error(), "private.person") {
			t.Errorf("the recipient would end up in the logs: %v", err)
		}
	})
	t.Run("the deadline stops the wait between attempts", func(t *testing.T) {
		mailer, _ := fakeResend(t, 500, 500, 500)
		mailer.Backoff = time.Minute
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := mailer.Send(ctx, "a@test.dev", "s", "b"); err == nil {
			t.Error("no error")
		}
		if time.Since(start) > 5*time.Second {
			t.Error("it kept waiting past the deadline")
		}
	})
}

// This one sends a real email, using RESEND_API_KEY, MAIL_FROM and LIVE_TEST_EMAIL from .env:
//
//	LIVE_TESTS=1 go test ./internal/notify -run Live -v
func TestLiveResend(t *testing.T) {
	if os.Getenv("LIVE_TESTS") != "1" {
		t.Skip("set LIVE_TESTS=1 to send a real email")
	}
	_ = godotenv.Load("../../.env")
	key, from, to := os.Getenv("RESEND_API_KEY"), os.Getenv("MAIL_FROM"), os.Getenv("LIVE_TEST_EMAIL")
	if key == "" || from == "" || to == "" {
		t.Fatal("LIVE_TESTS=1 needs RESEND_API_KEY, MAIL_FROM and LIVE_TEST_EMAIL in .env")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mailer := &Resend{Key: key, From: from, Base: "https://api.resend.com"}
	if err := mailer.Send(ctx, to, "wearwhat live test", "If you can read this, wearwhat can send email through Resend."); err != nil {
		t.Fatal(err)
	}
	t.Logf("sent: check the inbox for %s", to)
}
