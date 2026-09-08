package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	commonauth "github.com/sweeney/identity/common/auth"
)

// TokenParser is the interface that JWKSVerifier implements.
type TokenParser = commonauth.TokenParser

// VerifierMetrics is a point-in-time snapshot of a JWKSVerifier's counters and
// cache state (re-exported from common/auth for handlers that surface it).
type VerifierMetrics = commonauth.VerifierMetrics

type contextKey string

const claimsContextKey contextKey = "auth_claims"
const serviceClaimsContextKey contextKey = "service_claims"

// ClaimsFromContext extracts TokenClaims from a request context set by RequireAuth.
// Returns nil if not present (e.g. for service tokens, which are stored separately).
func ClaimsFromContext(ctx context.Context) *commonauth.TokenClaims {
	v := ctx.Value(claimsContextKey)
	if v == nil {
		return nil
	}
	c, _ := v.(*commonauth.TokenClaims)
	return c
}

// ServiceClaimsFromContext extracts ServiceTokenClaims from a request context set by RequireAuth.
// Returns nil if not present (e.g. for user tokens).
func ServiceClaimsFromContext(ctx context.Context) *commonauth.ServiceTokenClaims {
	v := ctx.Value(serviceClaimsContextKey)
	if v == nil {
		return nil
	}
	c, _ := v.(*commonauth.ServiceTokenClaims)
	return c
}

// RequireAuth validates the Bearer token and injects claims into the request
// context. Returns 401 for missing/invalid tokens, 403 for inactive accounts,
// and 503 when the token could not be checked at all (see writeTokenError).
func RequireAuth(parser TokenParser, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing authorization header")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid authorization header format")
			return
		}

		token := parts[1]
		if peekJWTTyp(token) == "at+jwt" {
			svcClaims, svcErr := parser.ParseServiceToken(r.Context(), token)
			if svcErr != nil || svcClaims == nil {
				writeTokenError(w, svcErr)
				return
			}
			ctx := context.WithValue(r.Context(), serviceClaimsContextKey, svcClaims)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		claims, err := parser.Parse(r.Context(), token)
		if err != nil {
			writeTokenError(w, err)
			return
		}

		if !claims.IsActive {
			writeError(w, http.StatusForbidden, "account_disabled", "account has been disabled")
			return
		}

		ctx := context.WithValue(r.Context(), claimsContextKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeTokenError answers a token that could not be verified.
//
// ErrKeysUnavailable means we could not check the token at all — identity was
// unreachable — which is a statement about our infrastructure, not a verdict on
// the caller. Answering 401 there tells every client to sign out during an
// identity outage, and they cannot sign back in until it recovers. 503 tells
// them to retry, which is both true and survivable.
//
// Everything else, including a nil error (claims absent with nothing to
// inspect), stays 401: an invalid or expired token genuinely is the caller's
// problem.
func writeTokenError(w http.ResponseWriter, err error) {
	if errors.Is(err, commonauth.ErrKeysUnavailable) {
		// Longer than the verifier's refetch throttle, so a compliant client
		// comes back to a fetch that will actually be attempted.
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "keys_unavailable",
			"cannot verify tokens right now — identity is unreachable; retry shortly")
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
}

func peekJWTTyp(token string) string {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return ""
	}
	var h struct {
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return ""
	}
	return h.Typ
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"error":   code,
		"message": message,
	})
}
