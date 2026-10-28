package httputil

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// WithSecurityHeaders wraps an http.Handler with standard HTTP method allow-listing (GET, POST, HEAD, OPTIONS)
// and hardened security response headers.
func WithSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodPost, http.MethodHead, http.MethodOptions:
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// WriteJSON encodes payload as JSON with no-store cache control and the given HTTP status code.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// EnvOr returns the trimmed environment variable value for key, or fallback if unset or empty.
func EnvOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// EnsureGoBinOnPath returns an environment slice with /usr/local/google/home/mofi/.local/go/bin and /usr/local/go/bin prepended to PATH.
func EnsureGoBinOnPath(baseEnv []string) []string {
	if len(baseEnv) == 0 {
		baseEnv = os.Environ()
	}
	curPath := os.Getenv("PATH")
	newPath := "/usr/local/google/home/mofi/.local/go/bin:/usr/local/go/bin:" + curPath
	return append(append([]string(nil), baseEnv...), "PATH="+newPath)
}
