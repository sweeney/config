// Package handler contains the HTTP handlers for the config service.
//
// Endpoints (all JSON):
//
//	GET    /api/v1/config                      → list visible namespaces
//	GET    /api/v1/config/{ns}                 → full document (requires read_role)
//	PUT    /api/v1/config/{ns}                 → replace document (requires write_role)
//	DELETE /api/v1/config/{ns}                 → delete namespace (admin-only)
//	POST   /api/v1/config/namespaces           → create namespace (admin-only)
//	PATCH  /api/v1/config/namespaces/{ns}      → update ACL (admin-only)
//	GET    /healthz                            → unauth health probe
package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, d.Version)
	})

	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(spec.JSON)
	})

	authed := func(h http.HandlerFunc) http.Handler {
		return auth.RequireAuth(d.Verifier, requireUserToken(h))
	}

	mux.Handle("GET /api/v1/config", authed(listHandler(d.Service)))
	mux.Handle("GET /api/v1/config/{ns}", authed(getHandler(d.Service)))
	mux.Handle("PUT /api/v1/config/{ns}", authed(putHandler(d.Service)))
	mux.Handle("DELETE /api/v1/config/{ns}", authed(deleteHandler(d.Service)))
	mux.Handle("POST /api/v1/config/namespaces", authed(createHandler(d.Service)))
	mux.Handle("PATCH /api/v1/config/namespaces/{ns}", authed(updateACLHandler(d.Service)))

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

func callerFromRequest(r *http.Request) service.Caller {
	if c := auth.ClaimsFromContext(r.Context()); c != nil {
		return service.Caller{Sub: c.UserID, Role: string(c.Role)}
	}
	sc := auth.ServiceClaimsFromContext(r.Context())
	return service.Caller{Sub: sc.ClientID, Role: domain.ConfigRoleUser}
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
		got, err := svc.Get(caller, ns)
		if err != nil {
			translateError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Read-Role", got.ReadRole)
		w.Header().Set("X-Write-Role", got.WriteRole)
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Vary", "Authorization")
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
			Name:      b.Name,
			ReadRole:  b.ReadRole,
			WriteRole: b.WriteRole,
			Document:  doc,
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
		if err := svc.UpdateACL(caller, ns, b.ReadRole, b.WriteRole); err != nil {
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

// --- helpers ---

func decodeBody(r *http.Request, into any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
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
			"role must be 'admin' or 'user'")
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
