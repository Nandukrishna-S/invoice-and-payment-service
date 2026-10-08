CREATE TABLE businesses (
    id         UUID PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id          UUID PRIMARY KEY,
    business_id UUID NOT NULL REFERENCES businesses (id),
    prefix      TEXT NOT NULL,
    -- SHA-256 of the key; the key itself is never stored.
    key_hash    BYTEA NOT NULL UNIQUE CHECK (octet_length(key_hash) = 32),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
