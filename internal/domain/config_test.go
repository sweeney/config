package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sweeney/config/internal/domain"
)

// TestRoleRank pins the privilege ordering the whole ACL model rests on:
// public is weaker than user, which is weaker than admin. Both role
// predicates and every access decision derive from this ordering, so it is
// enumerated exhaustively rather than spot-checked.
func TestRoleRank(t *testing.T) {
	public, ok := domain.RoleRank(domain.ConfigRolePublic)
	assert.True(t, ok)
	user, ok := domain.RoleRank(domain.ConfigRoleUser)
	assert.True(t, ok)
	admin, ok := domain.RoleRank(domain.ConfigRoleAdmin)
	assert.True(t, ok)

	assert.Less(t, public, user, "public must rank below user")
	assert.Less(t, user, admin, "user must rank below admin")
}

func TestRoleRank_UnknownRole(t *testing.T) {
	for _, bad := range []string{"", "root", "Admin", "PUBLIC", "superuser"} {
		t.Run(bad, func(t *testing.T) {
			_, ok := domain.RoleRank(bad)
			assert.False(t, ok, "unknown role must not be ranked")
		})
	}
}

// TestIsValidReadRole and TestIsValidWriteRole enforce the asymmetry that
// makes public safe: a namespace may be readable by anyone, but nothing is
// ever anonymously writable.
func TestIsValidReadRole(t *testing.T) {
	cases := map[string]bool{
		"admin":     true,
		"user":      true,
		"public":    true,
		"":          false,
		"root":      false,
		"Public":    false,
		"anonymous": false,
	}
	for role, want := range cases {
		t.Run(role, func(t *testing.T) {
			assert.Equal(t, want, domain.IsValidReadRole(role))
		})
	}
}

func TestIsValidWriteRole(t *testing.T) {
	cases := map[string]bool{
		"admin":  true,
		"user":   true,
		"public": false,
		"":       false,
		"root":   false,
	}
	for role, want := range cases {
		t.Run(role, func(t *testing.T) {
			assert.Equal(t, want, domain.IsValidWriteRole(role),
				"public must never be a valid write role — nothing is anonymously writable")
		})
	}
}
