package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hanleo-6/mailcat/internal/config"
)

func TestHealthEndpoint(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.Token = "secret-token"

	srv := New(cfg, nil)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

func TestBearerAuthMiddleware(t *testing.T) {
	const token = "super-secret-token"

	protected := BearerAuth(token)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	}))

	cases := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + token, http.StatusUnauthorized},
		{"lowercase bearer", "bearer " + token, http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong-token", http.StatusUnauthorized},
		{"extra whitespace", "Bearer  " + token, http.StatusUnauthorized},
		{"valid token", "Bearer " + token, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/outbox", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			if tc.wantStatus == http.StatusUnauthorized {
				var resp map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("401 body is not valid JSON: %v", err)
				}
				if resp["error"] == "" {
					t.Errorf("401 JSON missing non-empty error field: %v", resp)
				}
			}
		})
	}
}

func TestAuthenticatedRouteRequiresToken(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.Token = "valid-token"

	srv := New(cfg, nil)

	// Missing token -> 401 JSON.
	req := httptest.NewRequest(http.MethodGet, "/outbox", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", rec.Code)
	}

	// Wrong token -> 401 JSON.
	req = httptest.NewRequest(http.MethodGet, "/outbox", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", rec.Code)
	}

	// Valid token passes auth middleware (reaches stub handler, not 401).
	req = httptest.NewRequest(http.MethodGet, "/outbox", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid token unexpectedly got 401")
	}
}
