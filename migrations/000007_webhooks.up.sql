CREATE TABLE webhook_endpoints (
    id          UUID PRIMARY KEY,
    business_id UUID NOT NULL REFERENCES businesses (id),
    url         TEXT NOT NULL CHECK (char_length(url) BETWEEN 1 AND 2048),
    -- The signing secret (whsec_ + base64 of 32 random bytes). Signing needs the raw
    -- value, so it is stored as is; encrypting it at rest is a documented production gap.
    secret      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at TIMESTAMPTZ
);

-- Fan-out reads only the active endpoints of one business.
CREATE INDEX webhook_endpoints_active_business ON webhook_endpoints (business_id) WHERE disabled_at IS NULL;

-- The transactional outbox: one row per (event, endpoint), written in the same
-- transaction as the state change it describes, so an event exists if and only
-- if the change committed. Rows of one event share event_id.
CREATE TABLE webhook_deliveries (
    id              UUID PRIMARY KEY,
    event_id        TEXT NOT NULL,
    endpoint_id     UUID NOT NULL REFERENCES webhook_endpoints (id),
    type            TEXT NOT NULL CHECK (type IN ('invoice.created', 'invoice.paid', 'invoice.payment_failed')),
    -- The complete event body, exactly as it will be delivered.
    payload         JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempt_count   INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);

-- The delivery worker polls only the rows that are due.
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';
