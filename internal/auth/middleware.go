// Package auth adapts common/auth's JWKS token verification to this
// service's HTTP layer.
//
// It offers two middlewares, and choosing between them is a security
// decision:
//
//   - RequireAuth: a valid token is mandatory. This is the default, and the
//     right choice for every route unless there is a specific reason
//     otherwise.
//   - OptionalAuth: a request with no Authorization header proceeds
//     unauthenticated, and the handler must be able to answer safely for an
//     anonymous caller.
//
// OptionalAuth exists for exactly one route — GET of a single namespace,
// which may be published with read_role=public — because whether that route
// needs a token is a property of the namespace and cannot be known before
// the database lookup. It is safe there only because the service answers a
// namespace the caller cannot read with not-found. Do not reach for it to
// make a route "easier to call": a handler behind OptionalAuth receives no
// claims at all, so anything reading the caller's identity must cope with
// its absence rather than assume a token was checked upstream.
//
// Neither middleware ever treats a broken token as anonymous. See
// OptionalAuth's own documentation for why that matters.
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
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing authorization header")
			return
		}
		authenticate(parser, w, r, next)
	})
}

// OptionalAuth validates a Bearer token when one is presented, but lets a
// request carrying no Authorization header at all through unauthenticated.
//
// It exists for the single route that may be served anonymously: a GET of a
// namespace whose read_role is public. Whether a token is required is a
// property of the namespace, which lives in the database behind the service
// — so it cannot be known before the lookup. The middleware therefore has to
// admit the request and leave the access decision to the service, which
// already answers an unreadable namespace with not-found.
//
// This is deliberately not a general weakening. A token that is presented
// but does not verify is still rejected exactly as RequireAuth would reject
// it, including the 503 for an unreachable identity. Silently downgrading a
// broken token to anonymous would serve public namespaces to a client whose
// credentials had expired, hiding the failure until it touched something
// private. Only the total absence of the header means anonymous.
func OptionalAuth(parser TokenParser, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			next.ServeHTTP(w, r)
			return
		}
		authenticate(parser, w, r, next)
	})
}

// authenticate parses a presented Authorization header and, on success,
// calls next with the resulting claims in context. Shared by RequireAuth and
// OptionalAuth so the two cannot drift in how they judge a token; they
// differ only in what they do when no header was sent at all.
func authenticate(parser TokenParser, w http.ResponseWriter, r *http.Request, next http.Handler) {
	authHeader := r.Header.Get("Authorization")

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
