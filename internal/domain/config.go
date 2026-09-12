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

	// AuditActionDocumentWrite records that a namespace's contents changed.
	// The event only — never the body. Every write already ships the whole
	// database to R2, so keeping documents here would inflate both the
	// database and every backup without bound. To see what a document used
	// to contain, restore the backup from around that timestamp.
	AuditActionDocumentWrite = "document_write"
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

// AuditEntry is one recorded change to a namespace: its creation, its access
// rules, its contents, or its deletion. Document bodies are never recorded —
// this answers "who changed it, and when", not "what it said".
//
// Old roles are empty on create, new roles on delete, and both on a document
// write, which moves no roles.
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

	// UpdatedByUsername is who last modified this namespace — its document or
	// its ACL, since UpdateACL writes this too — by name as it stood then.
	// NULL in the database, so empty here, for callers without a username
	// (service tokens) and for rows written before the column existed;
	// UpdatedBy is always present.
	//
	// The same change is also in the audit trail, as a document_write or an
	// acl_change. This field is that answer without an admin token and
	// without reading a whole history.
	UpdatedByUsername string
}

// ConfigNamespaceSummary is returned by List — no document body.
type ConfigNamespaceSummary struct {
	Name      string
	ReadRole  string
	WriteRole string
	UpdatedAt time.Time

	// UpdatedByUsername only: the identity subject is deliberately not
	// carried here. The decision was to withhold it from the list, and a
	// populated field one line from the handler is an invitation to put it
	// back on the grounds that it is already there. ConfigNamespace keeps
	// UpdatedBy — the column is still written, and Get still returns it.
	UpdatedByUsername string
	CreatedAt         time.Time
}

// ConfigRepository is the persistence contract for config namespaces.
type ConfigRepository interface {
	List() ([]ConfigNamespaceSummary, error)
	GetACL(name string) (readRole, writeRole string, err error)
	Get(name string) (*ConfigNamespace, error)
	Create(ns *ConfigNamespace, actor Actor) error
	// UpdateDocument replaces the document and reports whether anything
	// changed. The comparison happens inside the write transaction: doing it
	// in a separate read let two concurrent writers of the same new content
	// both observe a difference, and the loser then recorded a document_write
	// for a change that did not happen.
	UpdateDocument(name string, document []byte, actor Actor, at time.Time) (changed bool, err error)
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
