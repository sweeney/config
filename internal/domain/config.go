package domain

import (
	"errors"
	"time"

	commonapierr "github.com/sweeney/identity/common/apierr"
)

var (
	ErrNotFound = commonapierr.ErrNotFound
	ErrConflict = commonapierr.ErrConflict

	// ErrPublishNotConfirmed is returned by a repository when an ACL update
	// would move read access to public but the caller did not confirm it.
	//
	// The decision lives in the repository rather than the service because it
	// depends on the namespace's current read role, and only the repository
	// can read that in the same transaction as the write. Deciding it from a
	// separately-read snapshot leaves a window in which a concurrent revoke
	// makes an unrelated edit look like "already public", and the guard is
	// skipped on a request that does in fact publish.
	ErrPublishNotConfirmed = errors.New("publishing this namespace was not confirmed")
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

// Audit actions recorded in the config_audit table.
const (
	AuditActionCreate    = "create"
	AuditActionACLChange = "acl_change"
	AuditActionDelete    = "delete"
)

// Actor is who performed a change: the identity subject, which is the stable
// key, and the username as it was at that moment.
//
// The username is stored rather than resolved when the trail is read. An
// audit record should not need identity to be reachable in order to be
// legible, and it should not change meaning because someone was later renamed
// or deleted — it says what was true at the time, which is the point of
// keeping one. Username may be empty: service tokens have no user, and rows
// written before the column existed do not know it. Readers fall back to Sub.
type Actor struct {
	Sub      string
	Username string
}

// AuditEntry is one recorded change to a namespace's existence or its access
// rules. Document writes are not audited and document bodies are never
// recorded — this answers "who changed the rules, and when", not "what was
// in it".
//
// Old roles are empty on create; new roles are empty on delete.
type AuditEntry struct {
	ID            int64
	Namespace     string
	Action        string
	OldReadRole   string
	OldWriteRole  string
	NewReadRole   string
	NewWriteRole  string
	Actor         string
	ActorUsername string
	At            time.Time
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

	// UpdatedByUsername is who last wrote this namespace, by name as it stood
	// then. Empty for callers without one — service tokens — and for rows
	// written before the column existed. Document writes are not audited, so
	// this is the only record of who last changed the contents.
	UpdatedByUsername string
}

// ConfigNamespaceSummary is returned by List — no document body.
type ConfigNamespaceSummary struct {
	Name              string
	ReadRole          string
	WriteRole         string
	UpdatedAt         time.Time
	UpdatedBy         string
	UpdatedByUsername string
	CreatedAt         time.Time
}

// ConfigRepository is the persistence contract for config namespaces.
type ConfigRepository interface {
	List() ([]ConfigNamespaceSummary, error)
	GetACL(name string) (readRole, writeRole string, err error)
	Get(name string) (*ConfigNamespace, error)
	Create(ns *ConfigNamespace, actor Actor) error
	UpdateDocument(name string, document []byte, actor Actor, at time.Time) error
	// UpdateACL replaces the ACL. publishConfirmed reports whether the caller
	// supplied a valid confirmation; implementations must compare the
	// incoming read role against the stored one inside the write transaction
	// and return ErrPublishNotConfirmed when an unconfirmed publish would
	// result.
	UpdateACL(name, readRole, writeRole string, actor Actor, at time.Time, publishConfirmed bool) error
	Delete(name string, actor Actor, at time.Time) error

	// ListAudit returns the recorded history for one namespace, oldest
	// first. Entries survive deletion of the namespace they describe.
	ListAudit(namespace string) ([]AuditEntry, error)
}

// Implementations of ConfigRepository must write the audit entry for a
// mutation in the same transaction as the mutation itself. A trail with
// gaps is worse than no trail, because it will be believed.

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
