package memory

import "github.com/cambrian-sh/core/domain"

// ErrorKind and IsErrorOutput are defined in domain/error_signal.go.
// Re-exported here so callers inside the memory package can use the short name.
type ErrorKind = domain.ErrorKind

const (
	ErrorKindBlocked     = domain.ErrorKindBlocked
	ErrorKindNonZeroExit = domain.ErrorKindNonZeroExit
	ErrorKindStderr      = domain.ErrorKindStderr
	ErrorKindException   = domain.ErrorKindException
	ErrorKindGeneric     = domain.ErrorKindGeneric
	ErrorKindRuntime     = domain.ErrorKindRuntime
)

// IsErrorOutput delegates to domain.IsErrorOutput.
func IsErrorOutput(text string) (bool, ErrorKind) {
	return domain.IsErrorOutput(text)
}
