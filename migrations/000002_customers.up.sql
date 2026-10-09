CREATE TABLE customers (
    id          UUID PRIMARY KEY,
    business_id UUID NOT NULL REFERENCES businesses (id),
    name        TEXT NOT NULL,
    email       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Target of invoices' tenant-safe composite foreign key.
    UNIQUE (business_id, id)
);

-- Serves the keyset list: newest first within one business.
CREATE INDEX customers_business_id_id_desc ON customers (business_id, id DESC);
