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
)
