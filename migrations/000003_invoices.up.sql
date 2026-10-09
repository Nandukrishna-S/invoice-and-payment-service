CREATE TABLE invoices (
    id                      UUID PRIMARY KEY,
    business_id             UUID NOT NULL REFERENCES businesses (id),
    customer_id             UUID NOT NULL,
    status                  TEXT NOT NULL
        CHECK (status IN ('draft', 'open', 'paid', 'void', 'uncollectible')),
    currency                TEXT NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
    total_cents             BIGINT NOT NULL CHECK (total_cents > 0),
    due_date                DATE NOT NULL,
    -- Assigned at finalize (checkpoint 7); null while draft.
    invoice_sequence_number TEXT
        CHECK (char_length(invoice_sequence_number) <= 16 AND invoice_sequence_number ~ '^[A-Za-z0-9/-]+$'),
    paid_at                 TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A composite key makes a cross-tenant customer reference impossible even if the app has a bug.
    CONSTRAINT invoices_customer_fk FOREIGN KEY (business_id, customer_id)
        REFERENCES customers (business_id, id),
    CONSTRAINT invoices_paid_at_matches_status CHECK ((status = 'paid') = (paid_at IS NOT NULL))
);

CREATE INDEX invoices_business_status_id ON invoices (business_id, status, id DESC);
CREATE INDEX invoices_business_id_id ON invoices (business_id, id DESC);

CREATE TABLE invoice_line_items (
    invoice_id        UUID NOT NULL REFERENCES invoices (id),
    position          INT NOT NULL CHECK (position > 0),
    description       TEXT NOT NULL,
    quantity          BIGINT NOT NULL CHECK (quantity > 0),
    unit_amount_cents BIGINT NOT NULL CHECK (unit_amount_cents > 0),
    amount_cents      BIGINT NOT NULL,
    PRIMARY KEY (invoice_id, position),
    -- The database re-verifies the arithmetic the app already did.
    CONSTRAINT line_item_amount_is_quantity_times_unit CHECK (amount_cents = quantity * unit_amount_cents)
);

-- Append-only audit of every status change. from_status is null for the creation row.
CREATE TABLE invoice_transitions (
    id          UUID PRIMARY KEY,
    invoice_id  UUID NOT NULL REFERENCES invoices (id),
    from_status TEXT CHECK (from_status IN ('draft', 'open', 'paid', 'void', 'uncollectible')),
    to_status   TEXT NOT NULL CHECK (to_status IN ('draft', 'open', 'paid', 'void', 'uncollectible')),
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX invoice_transitions_invoice_id_id ON invoice_transitions (invoice_id, id);
