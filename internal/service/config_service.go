package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/sweeney/config/internal/domain"
)

const MaxConfigDocumentBytes = 64 * 1024
const MaxConfigDocumentDepth = 64

var configNameRE = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// Caller describes the authenticated principal making a config request.
type Caller struct {
	Sub  string
	Role string
}

// ConfigService is the business logic layer for the config service.
type ConfigService struct {
	repo   domain.ConfigRepository
	backup domain.BackupService
	now    func() time.Time
}

func NewConfigService(repo domain.ConfigRepository, backup domain.BackupService) *ConfigService {
	return &ConfigService{
		repo:   repo,
		backup: backup,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (s *ConfigService) ListVisible(caller Caller) ([]domain.ConfigNamespaceSummary, error) {
	all, err := s.repo.List()
	if err != nil {
		return nil, err
	}
	visible := make([]domain.ConfigNamespaceSummary, 0, len(all))
	for _, ns := range all {
		if roleAllows(ns.ReadRole, caller.Role) {
			visible = append(visible, ns)
		}
	}
	return visible, nil
}

// Get returns the full namespace if caller has read access. Returns
// ErrConfigNamespaceNotFound for both missing namespaces and insufficient
// role so namespace existence is not leaked.
func (s *ConfigService) Get(caller Caller, name string) (*domain.ConfigNamespace, error) {
	if !configNameRE.MatchString(name) {
		return nil, ErrConfigInvalidName
	}
	readRole, _, err := s.repo.GetACL(name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, ErrConfigNamespaceNotFound
		}
		return nil, err
	}
	if !roleAllows(readRole, caller.Role) {
		return nil, ErrConfigNamespaceNotFound
	}
	ns, err := s.repo.Get(name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, ErrConfigNamespaceNotFound
		}
		return nil, err
	}
	return ns, nil
}

type CreateNamespaceInput struct {
	Name      string
	ReadRole  string
	WriteRole string
	Document  []byte

	// ConfirmPublic must equal Name when ReadRole is public. See
	// ErrConfigPublicConfirmRequired.
	ConfirmPublic string
}

// UpdateACLInput carries a replacement ACL. Both roles are always required:
// a PATCH rewrites the pair, it does not merge into it.
type UpdateACLInput struct {
	ReadRole  string
	WriteRole string

	// ConfirmPublic must equal the namespace name when this update moves
	// ReadRole to public. It is not required when the namespace is already
	// public, nor when revoking. See ErrConfigPublicConfirmRequired.
	ConfirmPublic string
}

func (s *ConfigService) CreateNamespace(caller Caller, in CreateNamespaceInput) (*domain.ConfigNamespace, error) {
	if caller.Role != domain.ConfigRoleAdmin {
		return nil, ErrConfigForbidden
	}
	if !configNameRE.MatchString(in.Name) {
		return nil, ErrConfigInvalidName
	}
	if !domain.IsValidReadRole(in.ReadRole) || !domain.IsValidWriteRole(in.WriteRole) {
		return nil, ErrConfigInvalidRole
	}
	if !writersAreReaders(in.ReadRole, in.WriteRole) {
		return nil, ErrConfigInvalidRole
	}
	if in.ReadRole == domain.ConfigRolePublic && in.ConfirmPublic != in.Name {
		return nil, ErrConfigPublicConfirmRequired
	}
	normalizedDoc, err := validateDocument(in.Document)
	if err != nil {
		return nil, err
	}

	now := s.now()
	ns := &domain.ConfigNamespace{
		Name:      in.Name,
		ReadRole:  in.ReadRole,
		WriteRole: in.WriteRole,
		Document:  normalizedDoc,
		UpdatedAt: now,
		UpdatedBy: caller.Sub,
		CreatedAt: now,
	}
	if err := s.repo.Create(ns); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil, ErrConfigNamespaceExists
		}
		return nil, err
	}

	s.fireBackup()
	return ns, nil
}

// PutDocument replaces the document for an existing namespace. Returns
// (changed, error). When changed is false no write occurred.
func (s *ConfigService) PutDocument(caller Caller, name string, document []byte) (bool, error) {
	if !configNameRE.MatchString(name) {
		return false, ErrConfigInvalidName
	}
	normalizedDoc, err := validateDocument(document)
	if err != nil {
		return false, err
	}

	readRole, writeRole, err := s.repo.GetACL(name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, ErrConfigNamespaceNotFound
		}
		return false, err
	}
	if !roleAllows(writeRole, caller.Role) {
		if !roleAllows(readRole, caller.Role) {
			return false, ErrConfigNamespaceNotFound
		}
		return false, ErrConfigForbidden
	}

	existing, err := s.repo.Get(name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, ErrConfigNamespaceNotFound
		}
		return false, err
	}
	if bytes.Equal(existing.Document, normalizedDoc) {
		return false, nil
	}

	if err := s.repo.UpdateDocument(name, normalizedDoc, caller.Sub, s.now()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, ErrConfigNamespaceNotFound
		}
		return false, err
	}
	s.fireBackup()
	return true, nil
}

// UpdateACL replaces a namespace's ACL. Publishing — moving read access to
// public — additionally requires in.ConfirmPublic to echo the namespace
// name; see ErrConfigPublicConfirmRequired.
func (s *ConfigService) UpdateACL(caller Caller, name string, in UpdateACLInput) error {
	if caller.Role != domain.ConfigRoleAdmin {
		return ErrConfigForbidden
	}
	if !configNameRE.MatchString(name) {
		return ErrConfigInvalidName
	}
	if !domain.IsValidReadRole(in.ReadRole) || !domain.IsValidWriteRole(in.WriteRole) {
		return ErrConfigInvalidRole
	}
	if !writersAreReaders(in.ReadRole, in.WriteRole) {
		return ErrConfigInvalidRole
	}

	// Read the current ACL before deciding: the confirmation guards the
	// transition into public, not the state of being public. Fetching it
	// first also means a missing namespace reports as not-found rather than
	// as a confirmation failure, so the guard is not an existence oracle.
	oldReadRole, _, err := s.repo.GetACL(name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ErrConfigNamespaceNotFound
		}
		return err
	}
	publishing := in.ReadRole == domain.ConfigRolePublic && oldReadRole != domain.ConfigRolePublic
	if publishing && in.ConfirmPublic != name {
		return ErrConfigPublicConfirmRequired
	}

	if err := s.repo.UpdateACL(name, in.ReadRole, in.WriteRole, caller.Sub, s.now()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ErrConfigNamespaceNotFound
		}
		return err
	}
	s.fireBackup()
	return nil
}

// ListAudit returns a namespace's recorded history, oldest first.
//
// Admin-only, and deliberately not gated by the namespace's read role: the
// document and its history are different things. A public namespace is
// world-readable, but who has been changing its access rules, and when it
// became public, is operational detail — and every operation the trail
// records is admin-only already, so anything weaker would leak more through
// the history than through the resource.
//
// An unknown namespace returns an empty history rather than not-found.
// Entries outlive the namespace they describe, so "what happened to the one
// that is no longer here" is precisely the question this answers, and a
// not-found would destroy it. Nothing leaks by doing so — the caller is
// already an admin, who may list every namespace anyway.
func (s *ConfigService) ListAudit(caller Caller, name string) ([]domain.AuditEntry, error) {
	if caller.Role != domain.ConfigRoleAdmin {
		return nil, ErrConfigForbidden
	}
	if !configNameRE.MatchString(name) {
		return nil, ErrConfigInvalidName
	}
	return s.repo.ListAudit(name)
}

func (s *ConfigService) Delete(caller Caller, name string) error {
	if caller.Role != domain.ConfigRoleAdmin {
		return ErrConfigForbidden
	}
	if !configNameRE.MatchString(name) {
		return ErrConfigInvalidName
	}
	if err := s.repo.Delete(name, caller.Sub, s.now()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ErrConfigNamespaceNotFound
		}
		return err
	}
	s.fireBackup()
	return nil
}

func (s *ConfigService) fireBackup() {
	if s.backup != nil {
		s.backup.TriggerAsync()
	}
}

func validateDocument(doc []byte) ([]byte, error) {
	if len(doc) == 0 {
		return nil, ErrConfigInvalidDocument
	}
	if err := enforceJSONDepth(doc, MaxConfigDocumentDepth); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(doc, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigInvalidDocument, err)
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigInvalidDocument, err)
	}
	if len(out) > MaxConfigDocumentBytes {
		return nil, ErrConfigDocumentTooLarge
	}
	return out, nil
}

func enforceJSONDepth(doc []byte, maxDepth int) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrConfigInvalidDocument, err)
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				if depth > maxDepth {
					return ErrConfigInvalidDocument
				}
			case '}', ']':
				depth--
			}
		}
	}
}

// roleAllows reports whether a caller holding callerRole satisfies a
// requirement of required. Both must be known roles: an unrecognised role
// never satisfies anything, so a malformed claim fails closed.
func roleAllows(required, callerRole string) bool {
	callerRank, ok := domain.RoleRank(callerRole)
	if !ok {
		return false
	}
	requiredRank, ok := domain.RoleRank(required)
	if !ok {
		return false
	}
	return callerRank >= requiredRank
}

// writersAreReaders enforces that every writer of a namespace can also read
// it — the read bar must be no higher than the write bar. Without it, a
// write-but-not-read role could use PUT's byte-equality no-op detection as a
// read oracle for a document it is not allowed to see.
func writersAreReaders(readRole, writeRole string) bool {
	readRank, ok := domain.RoleRank(readRole)
	if !ok {
		return false
	}
	writeRank, ok := domain.RoleRank(writeRole)
	if !ok {
		return false
	}
	return readRank <= writeRank
}
