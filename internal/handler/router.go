// Package handler contains the HTTP handlers for the config service.
//
// Endpoints (all JSON):
//
//	GET    /api/v1/config                      → list visible namespaces
//	GET    /api/v1/config/{ns}                 → full document (read_role; anonymous if public)
//	PUT    /api/v1/config/{ns}                 → replace document (requires write_role)
//	DELETE /api/v1/config/{ns}                 → delete namespace (admin-only)
//	POST   /api/v1/config/namespaces           → create namespace (admin-only)
//	PATCH  /api/v1/config/namespaces/{ns}      → update ACL (admin-only)
//	GET    /api/v1/config/namespaces/{ns}/audit → ACL history (admin-only)
//	GET    /healthz                            → unauth health probe
//	GET    /openapi.json                       → OpenAPI spec as JSON
//	GET    /openapi.yaml                       → OpenAPI spec as YAML
package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/sweeney/config/internal/auth"
	"github.com/sweeney/config/internal/domain"
	"github.com/sweeney/config/internal/service"
	"github.com/sweeney/config/spec"
	"github.com/sweeney/config/ui"
)

const maxBodyBytes = 128 * 1024

// metricsProvider is implemented by *commonauth.JWKSVerifier; used to surface
// JWKS cache/fetch counters on /healthz when the configured verifier supports it.
type metricsProvider interface {
	Metrics() auth.VerifierMetrics
}

// Router exposes the config service's HTTP handlers.
type Router struct {
	mux *http.ServeMux
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

// Deps bundles the service and auth dependencies.
type Deps struct {
	Service           *service.ConfigService
	Verifier          auth.TokenParser
	Version           string
	IdentityPublicURL string
	OAuthClientID     string
}

func NewRouter(d Deps) *Router {
	mux := http.NewServeMux()

	// Deliberately stays green when identity is unreachable, even though every
	// authenticated request is then answering 503: deploy.sh gates deploys on
	// this endpoint, and restarting config on an unhealthy signal would discard
	// the cached JWKS keys that let it ride the outage out. The jwks counters
	// below are the signal to alert on. See docs/admin.md, "Identity coupling".
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"status": "ok", "version": d.Version}
		// The JWKSVerifier exposes cache/fetch counters; other TokenParser
		// implementations (e.g. in tests) may not, so surface them only if present.
		if mp, ok := d.Verifier.(metricsProvider); ok {
			resp["jwks"] = mp.Metrics()
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		// Conversion is validated at startup and cached, so this won't error
		// in practice; guard anyway rather than serving a partial document.
		j, err := spec.Converter.JSON()
		if err != nil {
			http.Error(w, "openapi spec unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The spec is public and read-only; allow any origin so browser-based
		// API tooling (Swagger UI, Stoplight, etc.) can fetch it.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(j)
	})

	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(spec.Converter.YAML())
	})

	authed := func(h http.HandlerFunc) http.Handler {
		return auth.RequireAuth(d.Verifier, requireUserToken(h))
	}

	// optionallyAuthed serves a request that carries no Authorization header
	// as the anonymous public caller. Only the single-namespace GET uses it:
	// the service answers a namespace the caller cannot read with not-found,
	// so an anonymous request reaches exactly the public ones.
	optionallyAuthed := func(h http.HandlerFunc) http.Handler {
		return auth.OptionalAuth(d.Verifier, allowAnonymous(h))
	}

	// The list route stays authenticated deliberately. Knowing a namespace's
	// name is the price of reading it anonymously, and nothing advertises the
	// names.
	mux.Handle("GET /api/v1/config", authed(listHandler(d.Service)))
	mux.Handle("GET /api/v1/config/{ns}", optionallyAuthed(getHandler(d.Service)))
	mux.Handle("PUT /api/v1/config/{ns}", authed(putHandler(d.Service)))
	mux.Handle("DELETE /api/v1/config/{ns}", authed(deleteHandler(d.Service)))
	mux.Handle("POST /api/v1/config/namespaces", authed(createHandler(d.Service)))
	mux.Handle("PATCH /api/v1/config/namespaces/{ns}", authed(updateACLHandler(d.Service)))
	// Authenticated even for a public namespace: publishing a document does
	// not publish its history.
	mux.Handle("GET /api/v1/config/namespaces/{ns}/audit", authed(auditHandler(d.Service)))

	if d.IdentityPublicURL != "" && d.OAuthClientID != "" {
		mountSPA(mux, d.IdentityPublicURL, d.OAuthClientID)
	}

	return &Router{mux: mux}
}

func mountSPA(mux *http.ServeMux, identityURL, clientID string) {
	indexBytes, err := ui.StaticFS.ReadFile("static/index.html")
	if err != nil {
		indexBytes = []byte("config admin UI assets missing")
	}
	indexTmpl, tmplErr := template.New("index").Parse(string(indexBytes))

	var indexRendered []byte
	if tmplErr == nil {
		var buf bytes.Buffer
		if err := indexTmpl.Execute(&buf, struct{ AssetVer string }{ui.AssetVersion}); err == nil {
			indexRendered = buf.Bytes()
		}
	}
	if indexRendered == nil {
		indexRendered = indexBytes
	}

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = w.Write(indexRendered)
	})

	staticSub, _ := fs.Sub(ui.StaticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", noListing(http.FileServer(http.FS(staticSub)))))

	mux.HandleFunc("GET /spa-config.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"identity_url": identityURL,
			"client_id":    clientID,
		})
	})
}

func noListing(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// requireUserToken accepts user tokens with a known role and service tokens
// (which are always treated as user role). Rejects unrecognised role claims.
func requireUserToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := auth.ClaimsFromContext(r.Context()); c != nil {
			role := string(c.Role)
			if role != domain.ConfigRoleAdmin && role != domain.ConfigRoleUser {
				writeErr(w, http.StatusForbidden, "forbidden", "unrecognised role in token")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if auth.ServiceClaimsFromContext(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}
		writeErr(w, http.StatusForbidden, "forbidden", "valid user or service token required")
	})
}

// allowAnonymous is the optional-auth counterpart to requireUserToken. A
// token that is present must still carry a role we recognise; a request with
// no token at all proceeds as the anonymous public caller.
//
// A token that was presented but did not verify never reaches here —
// OptionalAuth has already answered it 401 (or 503).
func allowAnonymous(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := auth.ClaimsFromContext(r.Context()); c != nil {
			role := string(c.Role)
			if role != domain.ConfigRoleAdmin && role != domain.ConfigRoleUser {
				writeErr(w, http.StatusForbidden, "forbidden", "unrecognised role in token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func callerFromRequest(r *http.Request) service.Caller {
	if c := auth.ClaimsFromContext(r.Context()); c != nil {
		return service.Caller{Sub: c.UserID, Role: string(c.Role)}
	}
	if sc := auth.ServiceClaimsFromContext(r.Context()); sc != nil {
		return service.Caller{Sub: sc.ClientID, Role: domain.ConfigRoleUser}
	}
	// Neither kind of claim: an unauthenticated request that OptionalAuth let
	// through. The nil check on the service claims is load-bearing — this
	// dereferenced them unconditionally when every route demanded a token
	// first, which would now be a panic rather than a 401.
	return service.Caller{Role: domain.ConfigRolePublic}
}

// --- handlers ---

func listHandler(svc *service.ConfigService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := callerFromRequest(r)
		list, err := svc.ListVisible(caller)
		if err != nil {
			translateError(w, err)
			return
		}
		type item struct {
			Name      string `json:"name"`
			ReadRole  string `json:"read_role"`
			WriteRole string `json:"write_role"`
			UpdatedAt string `json:"updated_at"`
			CreatedAt string `json:"created_at"`
		}
		out := make([]item, 0, len(list))
		for _, ns := range list {
			out = append(out, item{
				Name:      ns.Name,
				ReadRole:  ns.ReadRole,
				WriteRole: ns.WriteRole,
				UpdatedAt: ns.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
				CreatedAt: ns.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func getHandler(svc *service.ConfigService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		caller := callerFromRequest(r)

		// Set before the lookup so every exit carries them, the 404 included.
		// A 404 is heuristically cacheable (RFC 9111 4.2.2) and is the only
		// answer an anonymous caller ever gets for a private namespace, so
		// without these a shared cache can key it without regard to
		// Authorization and replay it to someone who can actually read that
		// namespace. The public branch below relaxes Cache-Control on its way
		// out; nothing else needs to think about it.
		w.Header().Set("Cache-Control", "private, no-store")
		addVary(w, "Authorization")

		got, err := svc.Get(caller, ns)
		if err != nil {
			translateError(w, err)
			return
		}
		anonymous := caller.Role == domain.ConfigRolePublic

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Read-Role", got.ReadRole)
		// The write role is withheld from anonymous callers. That a plain user
		// token suffices to write is a nudge toward where to point a stolen
		// one, and nobody without a token can act on it; the read role is
		// self-evident from the read having succeeded at all.
		if !anonymous {
			w.Header().Set("X-Write-Role", got.WriteRole)
		}

		if got.ReadRole == domain.ConfigRolePublic {
			// A published document is meant to be fetched from anywhere,
			// including a browser on an origin we have never heard of.
			// Withholding this protects nothing — server-to-server callers
			// ignore CORS entirely — it only breaks the browser half of the
			// audience. Expose-Headers goes with it: the CORS middleware only
			// sends that to allow-listed origins, so without it the header
			// set just above is unreadable from JS for exactly the audience
			// the wildcard exists for.
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Expose-Headers", "X-Read-Role, X-Write-Role")
		}
		if got.ReadRole == domain.ConfigRolePublic && anonymous {
			// Only the answer that actually was anonymous is shared-cacheable.
			// Vary would keep a token-bearing copy correct, but it would also
			// store one per distinct token and mark a response served to an
			// identified principal as shared-cacheable. Keying on the caller
			// also confines the revoke-latency window to the anonymous copies,
			// which are the only ones a revoke cannot reach anyway.
			//
			// The TTL is that window: flipping read_role back does not purge a
			// CDN or a browser cache, so keep it short.
			w.Header().Set("Cache-Control", "public, max-age=60")
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(got.Document)
	}
}

func putHandler(svc *service.ConfigService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		caller := callerFromRequest(r)

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds size limit")
			return
		}
		changed, err := svc.PutDocument(caller, ns, body)
		if err != nil {
			translateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    ns,
			"changed": changed,
		})
	}
}

func deleteHandler(svc *service.ConfigService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		caller := callerFromRequest(r)
		if err := svc.Delete(caller, ns); err != nil {
			translateError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type createBody struct {
	Name      string          `json:"name"`
	ReadRole  string          `json:"read_role"`
	WriteRole string          `json:"write_role"`
	Document  json.RawMessage `json:"document"`

	// ConfirmPublic must echo Name when ReadRole is public. See
	// service.ErrConfigPublicConfirmRequired.
	ConfirmPublic string `json:"confirm_public"`
}

func createHandler(svc *service.ConfigService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := callerFromRequest(r)

		var b createBody
		if err := decodeBody(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
			return
		}
		doc := []byte(b.Document)
		if len(doc) == 0 {
			doc = []byte(`{}`)
		}
		ns, err := svc.CreateNamespace(caller, service.CreateNamespaceInput{
			Name:          b.Name,
			ReadRole:      b.ReadRole,
			WriteRole:     b.WriteRole,
			Document:      doc,
			ConfirmPublic: b.ConfirmPublic,
		})
		if err != nil {
			translateError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{
			"name":       ns.Name,
			"read_role":  ns.ReadRole,
			"write_role": ns.WriteRole,
		})
	}
}

type aclBody struct {
	ReadRole  string `json:"read_role"`
	WriteRole string `json:"write_role"`

	// ConfirmPublic must echo the namespace name when this request moves
	// read access to public. Not required when it is already public, and
	// never required when revoking.
	ConfirmPublic string `json:"confirm_public"`
}

func updateACLHandler(svc *service.ConfigService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		caller := callerFromRequest(r)

		var b aclBody
		if err := decodeBody(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
			return
		}
		if err := svc.UpdateACL(caller, ns, service.UpdateACLInput{
			ReadRole:      b.ReadRole,
			WriteRole:     b.WriteRole,
			ConfirmPublic: b.ConfirmPublic,
		}); err != nil {
			translateError(w, err)
			return
		}
		w.Header().Set("X-Read-Role", b.ReadRole)
		w.Header().Set("X-Write-Role", b.WriteRole)
		writeJSON(w, http.StatusOK, map[string]string{
			"name":       ns,
			"read_role":  b.ReadRole,
			"write_role": b.WriteRole,
		})
	}
}

func auditHandler(svc *service.ConfigService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		caller := callerFromRequest(r)
		entries, err := svc.ListAudit(caller, ns)
		if err != nil {
			translateError(w, err)
			return
		}
		type item struct {
			Action string `json:"action"`
			// Omitted rather than sent empty: a create has no previous ACL
			// and a delete has no resulting one, and "absent" says that more
			// honestly than an empty string.
			OldReadRole  string `json:"old_read_role,omitempty"`
			OldWriteRole string `json:"old_write_role,omitempty"`
			NewReadRole  string `json:"new_read_role,omitempty"`
			NewWriteRole string `json:"new_write_role,omitempty"`
			Actor        string `json:"actor"`
			At           string `json:"at"`
		}
		out := make([]item, 0, len(entries))
		for _, e := range entries {
			out = append(out, item{
				Action:       e.Action,
				OldReadRole:  e.OldReadRole,
				OldWriteRole: e.OldWriteRole,
				NewReadRole:  e.NewReadRole,
				NewWriteRole: e.NewWriteRole,
				Actor:        e.Actor,
				At:           e.At.UTC().Format("2006-01-02T15:04:05.000Z"),
			})
		}
		// The history of a public namespace is not itself public.
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, out)
	}
}

// --- helpers ---

func decodeBody(r *http.Request, into any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// addVary appends to Vary instead of replacing it. The CORS middleware in
// cmd/server sets Vary: Origin before any handler runs, and a plain Set here
// dropped it — which, now that public namespaces carry a max-age, could let
// a shared cache serve a response to an origin it was not built for.
func addVary(w http.ResponseWriter, value string) {
	if existing := w.Header().Get("Vary"); existing != "" {
		w.Header().Set("Vary", existing+", "+value)
		return
	}
	w.Header().Set("Vary", value)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": message,
	})
}

func translateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrConfigNamespaceNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "namespace not found")
	case errors.Is(err, service.ErrConfigNamespaceExists):
		writeErr(w, http.StatusConflict, "conflict", "namespace already exists")
	case errors.Is(err, service.ErrConfigForbidden):
		writeErr(w, http.StatusForbidden, "forbidden", "insufficient role for this operation")
	case errors.Is(err, service.ErrConfigInvalidName):
		writeErr(w, http.StatusBadRequest, "invalid_name",
			"namespace name must match ^[a-z0-9_-]{1,64}$")
	case errors.Is(err, service.ErrConfigInvalidRole):
		writeErr(w, http.StatusBadRequest, "invalid_role",
			"read_role must be 'admin', 'user' or 'public'; write_role must be 'admin' or 'user'; "+
				"and read_role may be no stronger than write_role")
	case errors.Is(err, service.ErrConfigPublicConfirmRequired):
		writeErr(w, http.StatusBadRequest, "confirm_required",
			"making a namespace public requires confirm_public to equal the namespace name")
	case errors.Is(err, service.ErrConfigInvalidDocument):
		writeErr(w, http.StatusBadRequest, "invalid_document",
			"document must be a JSON object")
	case errors.Is(err, service.ErrConfigDocumentTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "document_too_large",
			"document exceeds size limit")
	default:
		writeErr(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

var _ http.Handler = (*Router)(nil)
