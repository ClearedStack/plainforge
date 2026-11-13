// Package pferr holds the three public error codes of Plainforge.
//
// Every error that can leave the process (in an HTTP body, in a Git status
// line or in a log line) is one of these three codes followed by a fixed,
// plain sentence. Messages never carry a host name, an address, a path or a
// credential.
package pferr

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// Code is one of the three public error codes.
type Code string

const (
	// Timeout: something outside took too long.
	Timeout Code = "E_TIMEOUT"
	// Parse: the data was broken.
	Parse Code = "E_PARSE"
	// NeedsSubtool: the data was valid and unfamiliar.
	NeedsSubtool Code = "E_NEEDS_SUBTOOL"
)

// Error is a public error: a code and a fixed message.
type Error struct {
	Code Code
	Msg  string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Msg }

// Parsef builds an E_PARSE error.
func Parsef(format string, a ...any) *Error {
	return &Error{Code: Parse, Msg: fmt.Sprintf(format, a...)}
}

// Needsf builds an E_NEEDS_SUBTOOL error.
func Needsf(format string, a ...any) *Error {
	return &Error{Code: NeedsSubtool, Msg: fmt.Sprintf(format, a...)}
}

// Timeoutf builds an E_TIMEOUT error.
func Timeoutf(format string, a ...any) *Error {
	return &Error{Code: Timeout, Msg: fmt.Sprintf(format, a...)}
}

// Public turns any error into a public one. An error that already is public
// is returned as it is. A deadline becomes E_TIMEOUT. Anything else becomes
// E_PARSE with the fallback sentence, so that no detail from the operating
// system or the network can leak.
func Public(err error, fallback string) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return Timeoutf("the request took too long")
	}
	return Parsef("%s", fallback)
}
