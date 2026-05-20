package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	commonauth "github.com/sweeney/identity/common/auth"
)

// TokenParser is the interface that JWKSVerifier implements.
type TokenParser = commonauth.TokenParser

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

// RequireAuth validates the Bearer token and injects claims into the request
// context. Returns 401 for missing/invalid tokens, 403 for inactive accounts.
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
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
				return
			}
			ctx := context.WithValue(r.Context(), serviceClaimsContextKey, svcClaims)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		claims, err := parser.Parse(r.Context(), token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
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
