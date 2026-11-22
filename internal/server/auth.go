package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// WriteJSON writes v as JSON with the given HTTP status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a standard {"error": "<message>"} JSON response.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

// BearerAuth returns middleware that requires Authorization: Bearer <token>,
// compared in constant time. Unauthorised requests receive a JSON 401.
func BearerAuth(token string) func(http.Handler) http.Handler {
	expected := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			hdr := r.Header.Get("Authorization")
			if !strings.HasPrefix(hdr, prefix) {
				WriteError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			provided := []byte(hdr[len(prefix):])
			if subtle.ConstantTimeCompare(provided, expected) != 1 {
				WriteError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
