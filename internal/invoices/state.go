package invoices

// allowedTransitions is the whole state machine. Every status change, from any
// code path, must be checked against it. Terminal states (paid, void) have no
// entry, so nothing can leave them.
//
// uncollectible -> paid is deliberate: a written-off invoice that is paid late
// is still money owed to the business.
var allowedTransitions = map[Status][]Status{
	StatusDraft:         {StatusOpen, StatusVoid},
	StatusOpen:          {StatusPaid, StatusVoid, StatusUncollectible},
	StatusUncollectible: {StatusPaid, StatusVoid},
}

func canTransition(from, to Status) bool {
	for _, t := range allowedTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// action names a user-triggered transition for error messages and the audit reason.
type action struct {
	name   string // used in "cannot <name> invoice in state <state>"
	target Status
	reason string // stored in invoice_transitions.reason
}

var (
	actionFinalize        = action{"finalize", StatusOpen, "finalized"}
	actionVoid            = action{"void", StatusVoid, "voided"}
	actionMarkUncollected = action{"mark_uncollectible", StatusUncollectible, "marked_uncollectible"}
)
