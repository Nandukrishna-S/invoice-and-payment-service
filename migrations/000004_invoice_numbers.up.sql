-- One counter per business per financial year (April to March).
CREATE TABLE invoice_number_sequences (
    business_id UUID NOT NULL REFERENCES businesses (id),
    -- The year the financial year starts in, e.g. 2026 for 2026-27.
    fiscal_year INT NOT NULL CHECK (fiscal_year BETWEEN 2000 AND 2099),
    -- At most 3 characters keeps "PREFIX-NNNNNN/YY-YY" within the 16-character limit.
    prefix      TEXT NOT NULL DEFAULT 'INV' CHECK (prefix ~ '^[A-Za-z0-9]{1,3}$'),
    last_value  BIGINT NOT NULL,
    PRIMARY KEY (business_id, fiscal_year),
    -- Six digits is all the 16-character format can hold; exhausting it fails the finalize.
    CONSTRAINT invoice_number_counter_range CHECK (last_value BETWEEN 1 AND 999999)
);

CREATE UNIQUE INDEX invoices_business_sequence_number
    ON invoices (business_id, invoice_sequence_number)
    WHERE invoice_sequence_number IS NOT NULL;
