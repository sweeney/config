package domain

import (
	"time"

	commonapierr "github.com/sweeney/identity/common/apierr"
)

var (
	ErrNotFound = commonapierr.ErrNotFound
	ErrConflict = commonapierr.ErrConflict
)

const (
	ConfigRoleAdmin = "admin"
	ConfigRoleUser  = "user"
)

// BackupService defines the interface for triggering database backups.
type BackupService interface {
	TriggerAsync()
}

// ConfigNamespace is a named bucket of configuration data. The entire
// namespace is stored as a single JSON document; per-namespace role ACLs
// govern read and write access. Callers who lack the read role receive 404
// rather than 403, so namespace existence is not leaked.
type ConfigNamespace struct {
	Name      string
	ReadRole  string
	WriteRole string
	Document  []byte
	UpdatedAt time.Time
	UpdatedBy string
	CreatedAt time.Time
}

// ConfigNamespaceSummary is returned by List — no document body.
type ConfigNamespaceSummary struct {
	Name      string
	ReadRole  string
	WriteRole string
	UpdatedAt time.Time
	CreatedAt time.Time
}

// ConfigRepository is the persistence contract for config namespaces.
type ConfigRepository interface {
	List() ([]ConfigNamespaceSummary, error)
	GetACL(name string) (readRole, writeRole string, err error)
	Get(name string) (*ConfigNamespace, error)
	Create(ns *ConfigNamespace) error
	UpdateDocument(name string, document []byte, updatedBy string, at time.Time) error
	UpdateACL(name, readRole, writeRole, updatedBy string, at time.Time) error
	Delete(name string) error
}

// IsValidConfigRole reports whether role is one of the accepted ACL roles.
func IsValidConfigRole(role string) bool {
	return role == ConfigRoleAdmin || role == ConfigRoleUser
}
