-- One row per try at paying an invoice. It wraps a payments row (the money record)
-- with the invoice-side status. It carries no business_id: reads join invoices
-- and filter on invoices.business_id.
CREATE TABLE invoice_payment_attempts (
    id             UUID PRIMARY KEY,
    invoice_id     UUID NOT NULL REFERENCES invoices (id),
    payment_ref_id UUID NOT NULL UNIQUE REFERENCES payments (payment_ref_id),
    status         TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'succeeded', 'failed')),
    failure_code   TEXT CHECK (failure_code IN ('insufficient_funds', 'card_declined')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at    TIMESTAMPTZ,
    CONSTRAINT attempts_resolved_iff_not_pending CHECK ((status = 'pending') = (resolved_at IS NULL)),
    CONSTRAINT attempts_failure_code_iff_failed CHECK ((status = 'failed') = (failure_code IS NOT NULL)),
    -- Also the target of invoices.latest_payment_attempt_id, so that column can
    -- only ever point at an attempt of the same invoice. Serves "newest first" too.
    UNIQUE (invoice_id, id)
);

-- The database itself enforces one in-flight payment per invoice, even if some
-- code path forgets to take the invoice lock.
CREATE UNIQUE INDEX invoice_payment_attempts_one_pending
    ON invoice_payment_attempts (invoice_id) WHERE status = 'pending';

-- Written in the same transaction as the attempt it points at.
CREATE TABLE invoice_payment_idempotency_keys (
    business_id  UUID NOT NULL REFERENCES businesses (id),
    key          TEXT NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
    -- SHA-256 over invoice id, card token and amount. The token itself is never stored.
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    attempt_id   UUID NOT NULL UNIQUE REFERENCES invoice_payment_attempts (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (business_id, key)
);

ALTER TABLE invoices ADD COLUMN latest_payment_attempt_id UUID;
ALTER TABLE invoices ADD CONSTRAINT invoices_latest_attempt_fk
    FOREIGN KEY (id, latest_payment_attempt_id)
    REFERENCES invoice_payment_attempts (invoice_id, id);

ALTER TABLE invoice_transitions ADD COLUMN attempt_id UUID REFERENCES invoice_payment_attempts (id);
