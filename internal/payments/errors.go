package payments

import "errors"

var (
	ErrNotFound = errors.New("payment not found")
	// ErrNotDefinitive is returned when someone tries to resolve a payment with
	// an outcome that is not a definitive success or failure from the provider.
	// It guards the rule that uncertainty never becomes a final state.
	ErrNotDefinitive = errors.New("outcome is not a definitive provider result")
)
