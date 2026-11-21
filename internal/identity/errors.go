// Package identity is the client for the signed per-user identity document (account hash -> route id bindings) and for the
// account report that feeds it. It imports only the leaf remoteconfig package (envelope, signature check, trusted keys).
//
// A document is used only after its Ed25519 signature, payload rules, expiry and rollback floor have been verified; there is
// no unsigned path. Without a valid, unexpired document nothing resolves (fail closed). Errors carry a fixed Code and never
// document, response or account content.
package identity

import (
	"errors"

	"github.com/harnext/harnext-agent/internal/remoteconfig"
)

// Code is a fixed, content-free error code. Verification and transport codes are shared with remoteconfig.
type Code = remoteconfig.Code

const (
	// Verification codes, in check order (names match contracts/testdata/agent-identity.v1.vector.json).
	CodeMalformed      = remoteconfig.CodeMalformed
	CodeUnknownKID     = remoteconfig.CodeUnknownKID
	CodeBadSignature   = remoteconfig.CodeBadSignature
	CodeInvalidPayload = remoteconfig.CodeInvalidPayload
	CodeExpired        = remoteconfig.CodeExpired
	CodeRollback       = remoteconfig.CodeRollback

	CodeNoTrustedKeys    = remoteconfig.CodeNoTrustedKeys
	CodeUnauthorized     = remoteconfig.CodeUnauthorized
	CodeTransient        = remoteconfig.CodeTransient
	CodeUnexpectedStatus = remoteconfig.CodeUnexpectedStatus
	CodeTooLarge         = remoteconfig.CodeTooLarge
	CodeBadURL           = remoteconfig.CodeBadURL
	CodeNoToken          = remoteconfig.CodeNoToken

	// CodeRejected: the server refused the request body (400 or 413).
	CodeRejected Code = "rejected"
	// CodeInvalidReport: the account report broke a contract rule and was not sent.
	CodeInvalidReport Code = "invalid_report"
)

// Error is an error with a fixed code. It wraps nothing, so no URL, token or body can leak.
type Error struct {
	Code Code
	// RetryAfterSeconds is set for rate-limited responses (0 when absent).
	RetryAfterSeconds int
}

func (e *Error) Error() string { return "identity: " + string(e.Code) }

func newError(c Code) *Error { return &Error{Code: c} }

// CodeOf returns the code of err (this package's or remoteconfig's), or "" for any other error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return remoteconfig.CodeOf(err)
}

// IsTransient reports whether err is worth retrying with backoff.
func IsTransient(err error) bool { return CodeOf(err) == CodeTransient }
