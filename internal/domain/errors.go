// Package domain holds the core business types and rules. It depends on
// nothing else in the project, so every other layer can import it freely.
package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors. Layers wrap them with context using the helpers below and
// callers branch with errors.Is. The HTTP layer maps them to status codes:
//
//	ErrInvalid       -> 400
//	ErrNotFound      -> 404
//	ErrAlreadyExists -> 409
//	ErrConflict      -> 409
//
// Anything else is an unexpected failure and becomes a 500.
var (
	ErrInvalid       = errors.New("invalid input")
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrConflict      = errors.New("conflict")
)

// Invalidf returns an error that wraps ErrInvalid.
func Invalidf(format string, args ...any) error {
	return wrap(ErrInvalid, format, args...)
}

// NotFoundf returns an error that wraps ErrNotFound.
func NotFoundf(format string, args ...any) error {
	return wrap(ErrNotFound, format, args...)
}

// AlreadyExistsf returns an error that wraps ErrAlreadyExists.
func AlreadyExistsf(format string, args ...any) error {
	return wrap(ErrAlreadyExists, format, args...)
}

// Conflictf returns an error that wraps ErrConflict.
func Conflictf(format string, args ...any) error {
	return wrap(ErrConflict, format, args...)
}

func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...))
}
