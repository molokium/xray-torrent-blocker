package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newClient(t *testing.T, baseURL string, opts ...Option) *Client {
	t.Helper()
	client, err := New(baseURL, "pg_key_00000000-0000-4000-8000-000000000000", 2*time.Second, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestGetUserByID(t *testing.T) {
	var method, path, apiKey, authHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		apiKey = r.Header.Get("X-Api-Key")
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42,"username":"alice","status":"active","note":"tgid-123"}`)
	}))
	defer server.Close()

	user, err := newClient(t, server.URL).GetUserByID(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if method != http.MethodGet || path != "/api/user/by-id/42" {
		t.Errorf("request = %s %s", method, path)
	}
	if apiKey != "pg_key_00000000-0000-4000-8000-000000000000" {
		t.Errorf("X-Api-Key = %q", apiKey)
	}
	if authHeader != "" {
		t.Errorf("unexpected Authorization header %q", authHeader)
	}
	if user.ID != 42 || user.Username != "alice" || user.Status != "active" || user.Note != "tgid-123" {
		t.Errorf("user = %+v", user)
	}
}

func TestGetUserByUsernameEscapes(t *testing.T) {
	const username = "a@b.c_d-e"
	var captured string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/api/user/by-username/"
		if !strings.HasPrefix(r.URL.EscapedPath(), prefix) {
			t.Errorf("path = %q", r.URL.EscapedPath())
			http.NotFound(w, r)
			return
		}
		decoded, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), prefix))
		if err != nil {
			t.Errorf("unescape: %v", err)
		}
		captured = decoded
		_, _ = io.WriteString(w, `{"id":1,"username":"a@b.c_d-e","status":"active"}`)
	}))
	defer server.Close()

	if _, err := newClient(t, server.URL).GetUserByUsername(context.Background(), username); err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if captured != username {
		t.Errorf("username on wire = %q, want %q", captured, username)
	}
}

func TestSetDisabled(t *testing.T) {
	cases := []struct {
		name         string
		call         func(*Client) (*User, error)
		wantMethod   string
		wantPath     string
		wantDisabled bool
	}{
		{
			name: "by id",
			call: func(c *Client) (*User, error) {
				return c.SetDisabledByID(context.Background(), 7, true)
			},
			wantMethod:   http.MethodPut,
			wantPath:     "/api/user/by-id/7/disabled",
			wantDisabled: true,
		},
		{
			name: "by username enable",
			call: func(c *Client) (*User, error) {
				return c.SetDisabledByUsername(context.Background(), "alice", false)
			},
			wantMethod:   http.MethodPut,
			wantPath:     "/api/user/by-username/alice/disabled",
			wantDisabled: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var method, path string
			var body map[string]any

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":7,"username":"alice","status":"disabled"}`)
			}))
			defer server.Close()

			user, err := tc.call(newClient(t, server.URL))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if method != tc.wantMethod || path != tc.wantPath {
				t.Errorf("request = %s %s, want %s %s", method, path, tc.wantMethod, tc.wantPath)
			}
			if len(body) != 1 || body["disabled"] != tc.wantDisabled {
				t.Errorf("body = %#v", body)
			}
			if user.ID != 7 || user.Status != "disabled" {
				t.Errorf("user = %+v", user)
			}
		})
	}
}

func TestBearerAuthForJWT(t *testing.T) {
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"id":1,"username":"a","status":"active"}`)
	}))
	defer server.Close()

	client, err := New(server.URL, "eyJhbGciOiJIUzI1NiJ9.payload.sig", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetUserByID(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if authHeader != "Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig" {
		t.Errorf("Authorization = %q", authHeader)
	}
}

func TestAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"detail":"Not enough permissions"}`)
	}))
	defer server.Close()

	_, err := newClient(t, server.URL).GetUserByID(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T", err)
	}
	if apiErr.StatusCode != http.StatusForbidden || apiErr.Detail != "Not enough permissions" {
		t.Errorf("APIError = %+v", apiErr)
	}
	if !IsDefinitive(err) {
		t.Error("403 should be definitive")
	}
	if IsNotFound(err) {
		t.Error("403 must not be reported as 404")
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("error text = %q", err.Error())
	}
}

func TestNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"detail":"User not found"}`)
	}))
	defer server.Close()

	_, err := newClient(t, server.URL).GetUserByID(context.Background(), 999)
	if !IsNotFound(err) {
		t.Fatalf("expected 404, got %v", err)
	}
	if !IsDefinitive(err) {
		t.Error("404 should be definitive")
	}
}

func TestRetryOnServerError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"id":1,"username":"a","status":"disabled"}`)
	}))
	defer server.Close()

	client := newClient(t, server.URL, WithRetries(2), WithRetryBackoff(time.Millisecond))
	user, err := client.GetUserByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
	if user.Status != "disabled" {
		t.Errorf("user = %+v", user)
	}
}

func TestRetriesExhausted(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"detail":"boom"}`)
	}))
	defer server.Close()

	client := newClient(t, server.URL, WithRetries(1), WithRetryBackoff(time.Millisecond))
	_, err := client.GetUserByID(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
	if IsDefinitive(err) {
		t.Error("500 should not be definitive")
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	_, err := newClient(t, server.URL, WithRetries(3)).GetUserByID(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer server.Close()

	_, err := newClient(t, server.URL).GetUserByID(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want decode error", err)
	}
}

func TestBaseURLPrefix(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{"id":1,"username":"a","status":"active"}`)
	}))
	defer server.Close()

	if _, err := newClient(t, server.URL+"/panel/").GetUserByID(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if path != "/panel/api/user/by-id/1" {
		t.Errorf("path = %q", path)
	}
}

func TestCheck(t *testing.T) {
	t.Run("authenticated and permitted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/admin":
				_, _ = io.WriteString(w, `{"id":1,"username":"owner","status":"active"}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()
		if err := newClient(t, server.URL).Check(context.Background()); err != nil {
			t.Fatalf("Check: %v", err)
		}
	})

	t.Run("401 is an error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		if err := newClient(t, server.URL).Check(context.Background()); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("wrong base path is an error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<html>not found</html>")
		}))
		defer server.Close()
		if err := newClient(t, server.URL).Check(context.Background()); err == nil {
			t.Fatal("expected error for a non-PasarGuard 404")
		}
	})
}

func TestContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := newClient(t, server.URL, WithRetries(5)).GetUserByID(ctx, 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Errorf("cancellation took %s", elapsed)
	}
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		url    string
		apiKey string
	}{
		{"ftp://panel", "key"},
		{"://bad", "key"},
		{"https://", "key"},
		{"https://panel.example.com?x=1", "key"},
		{"https://panel.example.com", ""},
	}
	for _, tc := range cases {
		if _, err := New(tc.url, tc.apiKey, time.Second); err == nil {
			t.Errorf("New(%q, %q) succeeded, want error", tc.url, tc.apiKey)
		}
	}
}

func TestAPIErrorDetailVariants(t *testing.T) {
	if got := responseDetail([]byte(`{"detail":"plain"}`)); got != "plain" {
		t.Errorf("string detail = %q", got)
	}
	if got := responseDetail([]byte(`{"detail":[{"msg":"bad"}]}`)); !strings.Contains(got, "bad") {
		t.Errorf("structured detail = %q", got)
	}
	if got := responseDetail([]byte("  raw body  ")); got != "raw body" {
		t.Errorf("raw detail = %q", got)
	}
	long := strings.Repeat("x", 1000)
	if got := responseDetail([]byte(long)); len(got) > 404 {
		t.Errorf("long detail not truncated: %d", len(got))
	}
}

func TestRetryDelayCap(t *testing.T) {
	client := &Client{retryBase: time.Second}
	for i := 0; i < 10; i++ {
		if d := client.backoff(i); d > 5*time.Second {
			t.Fatalf("backoff(%d) = %s", i, d)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	fallback := 250 * time.Millisecond

	resp := &http.Response{Header: http.Header{"Retry-After": []string{"2"}}}
	if got := retryAfter(resp, fallback); got != 2*time.Second {
		t.Errorf("numeric Retry-After = %s", got)
	}

	resp.Header.Set("Retry-After", "600")
	if got := retryAfter(resp, fallback); got != 5*time.Second {
		t.Errorf("capped Retry-After = %s", got)
	}

	resp.Header.Set("Retry-After", "not-a-date")
	if got := retryAfter(resp, fallback); got != fallback {
		t.Errorf("invalid Retry-After = %s", got)
	}

	resp.Header.Set("Retry-After", time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat))
	if got := retryAfter(resp, fallback); got <= 0 || got > 5*time.Second {
		t.Errorf("date Retry-After = %s", got)
	}
}

func ExampleAPIError() {
	err := &APIError{StatusCode: 404, Method: "GET", Path: "/api/user/by-id/1", Detail: "User not found"}
	fmt.Println(err)
	// Output: PasarGuard API GET /api/user/by-id/1 returned HTTP 404: User not found
}

func TestNoRedirectHeaderLeak(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(true)
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusFound)
	}))
	defer source.Close()

	_, err := newClient(t, source.URL).GetUserByID(context.Background(), 1)
	if err == nil {
		t.Fatal("expected the redirect to be treated as an error")
	}
	if leaked.Load() {
		t.Fatal("API key headers were forwarded to the redirect target")
	}
}

func TestAPIKeyRedactedFromErrorDetail(t *testing.T) {
	const key = "pg_key_11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"bad key `+key+`"}`)
	}))
	defer server.Close()

	client, err := New(server.URL, key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetUserByID(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("API key leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("error was not marked redacted: %v", err)
	}
}

func TestNewRejectsEmbeddedCredentials(t *testing.T) {
	if _, err := New("https://user:pass@panel.example.com", "pg_key_x", time.Second); err == nil {
		t.Fatal("embedded credentials were accepted")
	}
}

func TestListEnabledNodes(t *testing.T) {
	var method, path, enabled string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		enabled = r.URL.Query().Get("enabled")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"nodes":[{"id":1,"name":"node-a","status":"connected"},{"id":2,"name":"node-b","status":"disabled"}],"total":2}`)
	}))
	defer server.Close()

	nodes, err := newClient(t, server.URL).ListEnabledNodes(context.Background())
	if err != nil {
		t.Fatalf("ListEnabledNodes: %v", err)
	}
	if method != http.MethodGet || path != "/api/nodes" || enabled != "true" {
		t.Errorf("request = %s %s enabled=%q", method, path, enabled)
	}
	if len(nodes) != 2 || nodes[0].ID != 1 || nodes[0].Name != "node-a" || nodes[1].Status != "disabled" {
		t.Errorf("nodes = %+v", nodes)
	}
}

func TestReconnectNode(t *testing.T) {
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	if err := newClient(t, server.URL).ReconnectNode(context.Background(), 7); err != nil {
		t.Fatalf("ReconnectNode: %v", err)
	}
	if method != http.MethodPost || path != "/api/nodes/7/reconnect" {
		t.Errorf("request = %s %s", method, path)
	}
}

func TestReconnectNodeRejectsInvalidID(t *testing.T) {
	client := newClient(t, "http://127.0.0.1:1")
	if err := client.ReconnectNode(context.Background(), 0); err == nil {
		t.Fatal("expected error")
	}
}
