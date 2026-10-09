-- The money record. It is domain-agnostic: no business_id and no invoice link.
-- Its owners are payment attempts, so reads go through invoices for tenant scoping.
-- There is deliberately no card token column: the service never stores it.
CREATE TABLE payments (
    -- Also the idempotency key sent to the PSP, so a retry can never double charge.
    payment_ref_id UUID PRIMARY KEY,
    amount_cents   BIGINT NOT NULL CHECK (amount_cents > 0),
    status         TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'succeeded', 'failed')),
    -- The PSP's reference, recorded on success.
    psp_ref_id     TEXT,
    failure_code   TEXT CHECK (failure_code IN ('insufficient_funds', 'card_declined')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ,
    CONSTRAINT payments_resolved_iff_not_pending CHECK ((status = 'pending') = (resolved_at IS NULL)),
    CONSTRAINT payments_failure_code_iff_failed CHECK ((status = 'failed') = (failure_code IS NOT NULL)),
    CONSTRAINT payments_psp_ref_iff_succeeded CHECK ((status = 'succeeded') = (psp_ref_id IS NOT NULL))
);

-- The reconciler scans only in-flight payments, oldest first.
CREATE INDEX payments_pending_created_at ON payments (created_at) WHERE status = 'pending';
