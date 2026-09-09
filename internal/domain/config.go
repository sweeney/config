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
	ConfigRoleAdmin  = "admin"
	ConfigRoleUser   = "user"
	ConfigRolePublic = "public"
)

// roleRanks orders the ACL roles from weakest to strongest. Every access
// decision reduces to comparing two ranks: a caller may read a namespace
// when its own rank is at least the namespace's required rank, and a
// namespace's read role may be no stronger than its write role.
//
// ConfigRolePublic is the bottom of the lattice. It is a valid read_role
// and the synthetic role given to unauthenticated callers; it is never a
// valid write_role and never appears as a claim in a token.
var roleRanks = map[string]int{
	ConfigRolePublic: 0,
	ConfigRoleUser:   1,
	ConfigRoleAdmin:  2,
}

// BackupService defines the interface for triggering database backups.
type BackupService interface {
	TriggerAsync()
}

// ConfigNamespace is a named bucket of configuration data. The entire
// namespace is stored as a single JSON document; per-namespace role ACLs
// govern read and write access. Callers who lack the read role receive 404
// rather than 403, so namespace existence is not leaked.
//
// A ReadRole of ConfigRolePublic makes the document readable without any
// token at all. WriteRole is never public.
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

// RoleRank returns the privilege rank of role, and whether role is known
// at all. Unknown roles are never ranked, so they can never satisfy a
// comparison by accident.
func RoleRank(role string) (int, bool) {
	rank, ok := roleRanks[role]
	return rank, ok
}

// IsValidReadRole reports whether role may be used as a namespace read_role.
// Every role in the lattice qualifies, including public.
func IsValidReadRole(role string) bool {
	_, ok := roleRanks[role]
	return ok
}

// IsValidWriteRole reports whether role may be used as a namespace
// write_role. Public is deliberately excluded: a namespace may be readable
// without a token, but nothing is ever writable without one.
func IsValidWriteRole(role string) bool {
	return role == ConfigRoleAdmin || role == ConfigRoleUser
}
