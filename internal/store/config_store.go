package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sweeney/config/db"
	"github.com/sweeney/config/internal/domain"
)

// ConfigStore is the SQLite-backed implementation of domain.ConfigRepository.
type ConfigStore struct {
	db *db.Database
}

func NewConfigStore(database *db.Database) *ConfigStore {
	return &ConfigStore{db: database}
}

func (s *ConfigStore) List() ([]domain.ConfigNamespaceSummary, error) {
	rows, err := s.db.DB().Query(
		`SELECT name, read_role, write_role, updated_at, created_at
		 FROM config_namespaces ORDER BY name ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list config namespaces: %w", err)
	}
	defer rows.Close()

	var out []domain.ConfigNamespaceSummary
	for rows.Next() {
		var (
			sum                  domain.ConfigNamespaceSummary
			updatedAt, createdAt string
		)
		if err := rows.Scan(&sum.Name, &sum.ReadRole, &sum.WriteRole, &updatedAt, &createdAt); err != nil {
			return nil, fmt.Errorf("scan config namespace: %w", err)
		}
		sum.UpdatedAt = parseTime(updatedAt)
		sum.CreatedAt = parseTime(createdAt)
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		// Never hand back a partial trail alongside an error. The whole point
		// of this table is that it can be believed.
		return nil, fmt.Errorf("iterate config audit: %w", err)
	}
	return out, nil
}

func (s *ConfigStore) GetACL(name string) (string, string, error) {
	row := s.db.DB().QueryRow(
		`SELECT read_role, write_role FROM config_namespaces WHERE name = ?`,
		name,
	)
	var readRole, writeRole string
	if err := row.Scan(&readRole, &writeRole); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", domain.ErrNotFound
		}
		return "", "", fmt.Errorf("get config acl: %w", err)
	}
	return readRole, writeRole, nil
}

func (s *ConfigStore) Get(name string) (*domain.ConfigNamespace, error) {
	row := s.db.DB().QueryRow(
		`SELECT name, read_role, write_role, document, updated_at, updated_by, created_at
		 FROM config_namespaces WHERE name = ?`,
		name,
	)
	var (
		ns                   domain.ConfigNamespace
		document             string
		updatedAt, createdAt string
	)
	err := row.Scan(&ns.Name, &ns.ReadRole, &ns.WriteRole, &document, &updatedAt, &ns.UpdatedBy, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("get config namespace: %w", err)
	}
	ns.Document = []byte(document)
	ns.UpdatedAt = parseTime(updatedAt)
	ns.CreatedAt = parseTime(createdAt)
	return &ns, nil
}

// withTx runs fn inside a single transaction, rolling back if it returns an
// error. Every mutation that also writes an audit row goes through here: a
// trail with gaps is worse than no trail, because it will be believed.
//
// fn must issue every statement on the tx it is handed. common/db pins the
// pool to a single connection (SetMaxOpenConns(1)), so reaching for
// s.db.DB() inside fn deadlocks rather than erroring — the transaction is
// holding the only connection, and the wait is silent. Neither the race
// detector nor a unit test will catch it.
//
// That same single connection is why the deferred transaction Begin() opens
// is safe here despite UpdateACL and Delete reading before they write: two
// writers serialise rather than racing to upgrade a shared lock. The safety
// comes from a dependency's pool setting, not from anything visible here.
func (s *ConfigStore) withTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.DB().Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// No-op once the transaction has been committed.
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// nullableRole maps an inapplicable role to NULL rather than to the empty
// string: "this event had no previous ACL" is different from "the previous
// ACL was blank".
func nullableRole(role string) any {
	if role == "" {
		return nil
	}
	return role
}

// insertAudit appends one audit row. It is always called BEFORE the mutation
// it describes, so that a mutation which fails — a CHECK violation, a
// unique-constraint conflict — takes the audit row down with it on rollback.
func insertAudit(tx *sql.Tx, e domain.AuditEntry) error {
	_, err := tx.Exec(
		`INSERT INTO config_audit
		   (namespace, action, old_read, old_write, new_read, new_write, actor, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Namespace,
		e.Action,
		nullableRole(e.OldReadRole),
		nullableRole(e.OldWriteRole),
		nullableRole(e.NewReadRole),
		nullableRole(e.NewWriteRole),
		e.Actor,
		formatTime(e.At),
	)
	if err != nil {
		return fmt.Errorf("append config audit: %w", err)
	}
	return nil
}

// readACLTx reads a namespace's current ACL inside the transaction, so the
// "old" roles recorded in the audit row are the ones actually being
// replaced, not a value the caller read moments earlier.
func readACLTx(tx *sql.Tx, name string) (readRole, writeRole string, err error) {
	err = tx.QueryRow(
		`SELECT read_role, write_role FROM config_namespaces WHERE name = ?`,
		name,
	).Scan(&readRole, &writeRole)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", domain.ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("read config acl: %w", err)
	}
	return readRole, writeRole, nil
}

func (s *ConfigStore) Create(ns *domain.ConfigNamespace) error {
	return s.withTx(func(tx *sql.Tx) error {
		if err := insertAudit(tx, domain.AuditEntry{
			Namespace:    ns.Name,
			Action:       domain.AuditActionCreate,
			NewReadRole:  ns.ReadRole,
			NewWriteRole: ns.WriteRole,
			Actor:        ns.UpdatedBy,
			At:           ns.CreatedAt,
		}); err != nil {
			return err
		}

		_, err := tx.Exec(
			`INSERT INTO config_namespaces
			   (name, read_role, write_role, document, updated_at, updated_by, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ns.Name,
			ns.ReadRole,
			ns.WriteRole,
			string(ns.Document),
			formatTime(ns.UpdatedAt),
			ns.UpdatedBy,
			formatTime(ns.CreatedAt),
		)
		if err != nil {
			if isUniqueConstraint(err) {
				return domain.ErrConflict
			}
			return fmt.Errorf("create config namespace: %w", err)
		}
		return nil
	})
}

func (s *ConfigStore) UpdateDocument(name string, document []byte, updatedBy string, at time.Time) error {
	res, err := s.db.DB().Exec(
		`UPDATE config_namespaces
		 SET document = ?, updated_at = ?, updated_by = ?
		 WHERE name = ?`,
		string(document),
		formatTime(at),
		updatedBy,
		name,
	)
	if err != nil {
		return fmt.Errorf("update config document: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *ConfigStore) UpdateACL(name, readRole, writeRole, updatedBy string, at time.Time, publishConfirmed bool) error {
	return s.withTx(func(tx *sql.Tx) error {
		oldRead, oldWrite, err := readACLTx(tx, name)
		if err != nil {
			return err
		}
		// Evaluated here, against the row this transaction is about to
		// overwrite, so a concurrent revoke cannot make a publish look like
		// an edit to something already public.
		if readRole == domain.ConfigRolePublic &&
			oldRead != domain.ConfigRolePublic && !publishConfirmed {
			return domain.ErrPublishNotConfirmed
		}
		if err := insertAudit(tx, domain.AuditEntry{
			Namespace:    name,
			Action:       domain.AuditActionACLChange,
			OldReadRole:  oldRead,
			OldWriteRole: oldWrite,
			NewReadRole:  readRole,
			NewWriteRole: writeRole,
			Actor:        updatedBy,
			At:           at,
		}); err != nil {
			return err
		}

		res, err := tx.Exec(
			`UPDATE config_namespaces
			 SET read_role = ?, write_role = ?, updated_at = ?, updated_by = ?
			 WHERE name = ?`,
			readRole,
			writeRole,
			formatTime(at),
			updatedBy,
			name,
		)
		if err != nil {
			return fmt.Errorf("update config acl: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

func (s *ConfigStore) Delete(name, deletedBy string, at time.Time) error {
	return s.withTx(func(tx *sql.Tx) error {
		oldRead, oldWrite, err := readACLTx(tx, name)
		if err != nil {
			return err
		}
		// New roles are left empty: a deleted namespace has no resulting ACL.
		if err := insertAudit(tx, domain.AuditEntry{
			Namespace:    name,
			Action:       domain.AuditActionDelete,
			OldReadRole:  oldRead,
			OldWriteRole: oldWrite,
			Actor:        deletedBy,
			At:           at,
		}); err != nil {
			return err
		}

		res, err := tx.Exec(`DELETE FROM config_namespaces WHERE name = ?`, name)
		if err != nil {
			return fmt.Errorf("delete config namespace: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// ListAudit returns the recorded history for one namespace, oldest first.
// Ordered by insertion rather than timestamp so entries recorded within the
// same clock tick keep their true order. Rows outlive the namespace they
// describe — see the note on the config_audit schema.
func (s *ConfigStore) ListAudit(namespace string) ([]domain.AuditEntry, error) {
	rows, err := s.db.DB().Query(
		`SELECT id, namespace, action, old_read, old_write, new_read, new_write, actor, at
		 FROM config_audit WHERE namespace = ? ORDER BY id ASC`,
		namespace,
	)
	if err != nil {
		return nil, fmt.Errorf("list config audit: %w", err)
	}
	defer rows.Close()

	var out []domain.AuditEntry
	for rows.Next() {
		var (
			e                                    domain.AuditEntry
			oldRead, oldWrite, newRead, newWrite sql.NullString
			at                                   string
		)
		if err := rows.Scan(&e.ID, &e.Namespace, &e.Action,
			&oldRead, &oldWrite, &newRead, &newWrite, &e.Actor, &at); err != nil {
			return nil, fmt.Errorf("scan config audit: %w", err)
		}
		e.OldReadRole = oldRead.String
		e.OldWriteRole = oldWrite.String
		e.NewReadRole = newRead.String
		e.NewWriteRole = newWrite.String
		e.At = parseTime(at)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		// Never hand back a partial trail alongside an error. The whole point
		// of this table is that it can be believed.
		return nil, fmt.Errorf("iterate config audit: %w", err)
	}
	return out, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		log.Printf("config_store: corrupted timestamp %q: %v", s, err)
	}
	return t
}

func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
