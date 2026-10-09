-- The mock PSP runs with search_path=psp, so this table lands in the psp schema.
CREATE TABLE charges (
    idempotency_key TEXT PRIMARY KEY CHECK (char_length(idempotency_key) BETWEEN 1 AND 255),
    psp_ref_id      TEXT NOT NULL UNIQUE,
    token           TEXT NOT NULL,
    amount_cents    BIGINT NOT NULL CHECK (amount_cents > 0),
    status          TEXT NOT NULL CHECK (status IN ('processing', 'succeeded', 'failed')),
    failure_code    TEXT CHECK (failure_code IN ('insufficient_funds', 'card_declined')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,
    CONSTRAINT failed_iff_failure_code CHECK ((status = 'failed') = (failure_code IS NOT NULL)),
    CONSTRAINT completed_iff_not_processing CHECK ((status = 'processing') = (completed_at IS NULL))
);
