package httpapi

import (
	"net/http"
	"strconv"
	"strings"
)

// CORSConfig describes which browser origins may call this API.
//
// The API is consumed by a separately-served single-page app, so without this
// the browser refuses to hand any response body to the frontend's JavaScript.
// A Vite dev proxy is an alternative, but the backend is also reachable
// directly (AI Studio applet, curl, Postman), so it should describe its own
// policy rather than depending on how a particular client is configured.
type CORSConfig struct {
	// AllowedOrigins lists exact origins, e.g. "https://app.example.com". A
	// single "*" allows any origin.
	AllowedOrigins []string
	// AllowCredentials permits cookies and Authorization headers. It cannot be
	// combined with a wildcard origin per the CORS spec, so a wildcard is
	// downgraded to echoing the request origin with credentials off.
	AllowCredentials bool
	// MaxAge is how long a browser may cache the preflight result.
	MaxAge int
}

// Allows reports whether origin is permitted, and the value to echo back.
//
// The echoed value is the request's own origin whenever credentials are in
// play: the spec forbids pairing a wildcard with credentials, and a browser
// discards a "Access-Control-Allow-Credentials: true" response that also
// carries "*", so the wildcard has to be resolved to a concrete origin.
func (c CORSConfig) Allows(origin string) (string, bool) {
	if origin == "" {
		return "", false
	}
	for _, allowed := range c.AllowedOrigins {
		if allowed == "*" {
			if c.AllowCredentials {
				return origin, true
			}
			return "*", true
		}
		if strings.EqualFold(allowed, origin) {
			return origin, true
		}
	}
	return "", false
}

// cors answers preflight requests and tags cross-origin responses.
//
// Only the headers this API actually uses are allowed. The request itself is a
// simple multipart POST, so a preflight is not strictly required today, but
// answering OPTIONS correctly keeps the API usable from clients that add an
// Authorization or Content-Type header, and preflight caching avoids a round
// trip per request.
func cors(cfg CORSConfig) Middleware {
	allowHeaders := "Accept, Content-Type, X-Request-ID"
	exposeHeaders := "X-Request-ID"

	// With credentials enabled the spec forbids "*", so echo the origin back.
	// Returning the concrete origin is what makes the response readable at all.
	allowCredentials := ""
	if cfg.AllowCredentials {
		allowCredentials = "true"
	}
	maxAge := ""
	if cfg.MaxAge > 0 {
		maxAge = strconv.Itoa(cfg.MaxAge)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			allowedOrigin, allowed := cfg.Allows(origin)

			// A preflight for a disallowed origin gets a bare 403 rather than
			// the normal error envelope: the browser is the caller here, and no
			// header can be set on the response without the very permission
			// being refused.
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				if !allowed {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", allowedOrigin)
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", allowHeaders)
				h.Set("Access-Control-Expose-Headers", exposeHeaders)
				h.Set("Access-Control-Max-Age", maxAgeStr(maxAge))
				if allowCredentials != "" {
					h.Set("Access-Control-Allow-Credentials", allowCredentials)
				}
				h.Add("Vary", "Origin")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			if allowed {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", allowedOrigin)
				h.Add("Vary", "Origin")
				if allowCredentials != "" && allowedOrigin != "*" {
					h.Set("Access-Control-Allow-Credentials", allowCredentials)
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// maxAgeStr renders the header value, omitting it when unset.
func maxAgeStr(v string) string {
	if v == "" {
		return "0"
	}
	return v
}
