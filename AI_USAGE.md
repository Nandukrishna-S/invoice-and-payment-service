# AI usage

## Tools and what I used them for

- **Claude (chat), design phase.** I used it as a sparring partner before writing any code: going through the brief, listing functional and non-functional requirements, the data model, the state machine, the payment flow and failure modes, and the error model. It also produced the first drafts of `DESIGN.md`, `openapi.yaml` and `CLAUDE.md`, which I then edited. I pushed back on many of its suggestions (below); several of its suggestions also changed my mind and treating a PSP timeout as *unknown* rather than *failed*.
- **Claude Code, implementation.** It wrote most of the Go code, migrations and tests, one checkpoint at a time, following a `CLAUDE.md` I wrote with hard rules: no git operations (I committed everything myself), ask before any design decision not already in `DESIGN.md`/`openapi.yaml`, no floats in the money path, no transaction held across a PSP call. At each checkpoint it ran fmt, vet, lint, `go test -race` and a review pass, and I reviewed its code, report and questions before approving the next step.
- **Verification passes.** I had Claude Code check the build against the requirements (e.g. the mock PSP's tokens, timings and response shape) and against `DESIGN.md`, reporting only, before I approved any fixes.

## Three decisions I made myself

1. **The client sends `expected_total_cents`, and the server verifies it.**
   AI proposed: no total field on the request at all, so a client total can't even be parsed.
   I chose: the client sends the total it displayed; the server computes the total from the line items with one pure function, compares, returns `422 total_mismatch` on a difference, and never stores the client value.
   Why: in a real UI the user sees a total before clicking "create". If the frontend and backend calculations ever diverge, the invoice should fail loudly instead of silently billing an amount the user never saw.

2. **A separate, domain-agnostic `payments` table.**
   AI proposed: a single payment-attempts table holding the PSP reference and status.
   I chose: `invoice_payment_attempts` (who tried to pay which invoice) separate from `payments` (money movement only: our reference sent to the PSP as the idempotency key, the PSP's reference, amount, status). `payments` is the source of truth for whether money moved.
   Why: the payment record shouldn't know about invoices. Another module (subscriptions, checkout) could reuse it, and the reconciler works on payments, not invoices.

3. **Errors carry the HTTP status directly.**
   AI proposed: a transport-agnostic error "kind" that the HTTP layer maps to a status code.
   I chose: `apperr.Error{Status, Code, Message, Err}` with a catalogue of errors per module, and a single writer that renders every error in the same envelope.
   Why: there is only one transport. The extra mapping layer added indirection without changing behaviour, and the brief rewards simple over clever.

Other decisions of mine: the framework, modules register their own routes (chi route groups) and specific middlewares while `main` owns  global middleware and workers, instead of a central composition root; handlers map DTOs and services take and return only domain types; invoice sequence numbers assigned at finalize (consecutive per business per financial year, per India's GST rule); and leaving a payment `pending` when the PSP has no record of it, rather than marking it failed after a timeout.

## Something the AI got wrong

**It named business logic a "handler".** When designing the reconciler, the AI wired it to billing's "PaymentHandler". I pointed out that the reconciler needs billing's business logic, not an HTTP handler, so it should call the billing service. The method moved to the service behind a small interface owned by the payments side, so the payments package never imports billing.

Other mistakes, caught by verification rather than by me directly:
- `DESIGN.md` said the webhook retries spanned "about 19.7 hours"; the schedule actually sums to about 19.2. Claude Code caught it by checking the doc against the code.
- The first mock PSP returned different field names from the brief and responded instantly instead of after ~100 ms. A verification pass against the brief caught it.
- Cluade Code ran tests that got the APis returning 500 in cases like the client cancelled the request midflight ..etc
