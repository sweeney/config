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
}

func (s *ConfigService) CreateNamespace(caller Caller, in CreateNamespaceInput) (*domain.ConfigNamespace, error) {
	if caller.Role != domain.ConfigRoleAdmin {
		return nil, ErrConfigForbidden
	}
	if !configNameRE.MatchString(in.Name) {
		return nil, ErrConfigInvalidName
	}
	if !domain.IsValidConfigRole(in.ReadRole) || !domain.IsValidConfigRole(in.WriteRole) {
		return nil, ErrConfigInvalidRole
	}
	if !writersAreReaders(in.ReadRole, in.WriteRole) {
		return nil, ErrConfigInvalidRole
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

func (s *ConfigService) UpdateACL(caller Caller, name, readRole, writeRole string) error {
	if caller.Role != domain.ConfigRoleAdmin {
		return ErrConfigForbidden
	}
	if !configNameRE.MatchString(name) {
		return ErrConfigInvalidName
	}
	if !domain.IsValidConfigRole(readRole) || !domain.IsValidConfigRole(writeRole) {
		return ErrConfigInvalidRole
	}
	if !writersAreReaders(readRole, writeRole) {
		return ErrConfigInvalidRole
	}
	if err := s.repo.UpdateACL(name, readRole, writeRole, caller.Sub, s.now()); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return ErrConfigNamespaceNotFound
		}
		return err
	}
	s.fireBackup()
	return nil
}

func (s *ConfigService) Delete(caller Caller, name string) error {
	if caller.Role != domain.ConfigRoleAdmin {
		return ErrConfigForbidden
	}
	if !configNameRE.MatchString(name) {
		return ErrConfigInvalidName
	}
	if err := s.repo.Delete(name); err != nil {
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

func roleAllows(required, callerRole string) bool {
	if callerRole == domain.ConfigRoleAdmin {
		return true
	}
	if required == domain.ConfigRoleUser && callerRole == domain.ConfigRoleUser {
		return true
	}
	return false
}

func writersAreReaders(readRole, writeRole string) bool {
	if writeRole == domain.ConfigRoleAdmin {
		return true
	}
	return readRole == domain.ConfigRoleUser
}
