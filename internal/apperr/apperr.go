// Package apperr defines the error type that handlers turn into the API's
// error envelope.
package apperr

import "fmt"

// Error carries everything the API response needs. Code is stable and matches
// openapi.yaml; Message is for humans and may change. Err is the underlying
// cause, logged but never sent to the client.
type Error struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Wrap returns a copy carrying cause, so the shared catalogue values are never
// mutated.
func (e *Error) Wrap(cause error) *Error {
	c := *e
	c.Err = cause
	return &c
}

// WithMessage returns a copy with a more specific message.
func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	return &c
}
