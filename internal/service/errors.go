package service

import "errors"

var (
	ErrConfigNamespaceNotFound = errors.New("config namespace not found")
	ErrConfigNamespaceExists   = errors.New("config namespace already exists")
	ErrConfigForbidden         = errors.New("config operation forbidden by namespace acl")
	ErrConfigInvalidName       = errors.New("invalid config namespace name")
	ErrConfigInvalidRole       = errors.New("invalid config role")
	ErrConfigInvalidDocument   = errors.New("config document must be a JSON object")
	ErrConfigDocumentTooLarge  = errors.New("config document exceeds size limit")

	// ErrConfigPublicConfirmRequired is returned when a namespace is being
	// made publicly readable without the caller echoing its name back. The
	// guard exists because publishing is the one ACL change whose effect
	// cannot be taken back: revoking the role stops future reads, but
	// anything already fetched is gone. Binding the confirmation to the
	// namespace name means a request body cannot be replayed against a
	// different namespace, and a stray dropdown cannot publish one as a
	// side effect of an unrelated edit.
	ErrConfigPublicConfirmRequired = errors.New("publishing a namespace requires confirmation")
)
