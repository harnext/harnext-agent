// Package remoteconfig fetches, verifies and stores the signed agent configuration
// (friction coefficients, patterns, topic rules). It is a leaf package: it imports no
// other internal package.
//
// A configuration is only used after its Ed25519 signature, schema, expiry and version
// have been verified (see Verify). There is no unsigned loading path and no built-in
// fallback configuration. Errors carry a fixed Code and never payload or response content.
package remoteconfig

import "errors"

// Code is a fixed, content-free error code.
type Code string

const (
	// Verification codes, in check order (names match contracts/testdata/agent-config.v1.vector.json).
	CodeMalformed        Code = "malformed"
	CodeUnknownKID       Code = "unknown_kid"
	CodeBadSignature     Code = "bad_signature"
	CodeInvalidPayload   Code = "invalid_payload"
	CodeExpired          Code = "expired"
	CodeRollback         Code = "rollback"
	CodeNoTrustedKeys    Code = "no_trusted_keys"
	CodeUnauthorized     Code = "unauthorized"
	CodeTransient        Code = "transient"
	CodeUnexpectedStatus Code = "unexpected_status"
	CodeTooLarge         Code = "response_too_large"
	CodeBadURL           Code = "bad_url"
	CodeNoToken          Code = "no_token"
)

// Error is an error with a fixed code. It wraps nothing, so no URL, token or body can leak.
type Error struct {
	Code Code
	// RetryAfterSeconds is set for rate-limited responses (0 when absent).
	RetryAfterSeconds int
}

func (e *Error) Error() string { return "remoteconfig: " + string(e.Code) }

func newError(c Code) *Error { return &Error{Code: c} }

// CodeOf returns the code of err, or "" if err is not (or does not wrap) an *Error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsTransient reports whether err is worth retrying with backoff.
func IsTransient(err error) bool { return CodeOf(err) == CodeTransient }
