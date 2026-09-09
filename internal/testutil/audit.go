// Package testutil holds test doubles shared between the service and handler
// test suites.
//
// The in-memory repository fakes themselves stay duplicated in each test
// file, so that each suite can be read without cross-referencing another
// package — see "Fake implementations" in docs/testing.md. What is shared
// here is only the part that must agree between them: what an audit entry
// looks like and when one is recorded. Two independent copies of that would
// drift, and drift in a fake makes tests pass that should not.
package testutil

import (
	"sync"

	"github.com/sweeney/config/internal/domain"
)

// AuditLog is an in-memory recorder of namespace audit entries.
//
// It models the shape and presence of entries only. Atomicity is
// deliberately NOT simulated: in a fake both the mutation and the audit
// write happen under one mutex, so a fake asserting rollback would only be
// asserting its own construction. The rollback guarantee is tested where it
// is real — against SQLite, in internal/store's integration suite.
type AuditLog struct {
	mu      sync.Mutex
	entries []domain.AuditEntry
}

// Record appends one entry, assigning a sequential ID as the store does.
func (l *AuditLog) Record(e domain.AuditEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.ID = int64(len(l.entries) + 1)
	l.entries = append(l.entries, e)
}

// List returns the entries for one namespace, oldest first. Entries survive
// deletion of the namespace they describe.
func (l *AuditLog) List(namespace string) []domain.AuditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]domain.AuditEntry, 0, len(l.entries))
	for _, e := range l.entries {
		if e.Namespace == namespace {
			out = append(out, e)
		}
	}
	return out
}

// All returns every recorded entry, oldest first.
func (l *AuditLog) All() []domain.AuditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.AuditEntry(nil), l.entries...)
}
