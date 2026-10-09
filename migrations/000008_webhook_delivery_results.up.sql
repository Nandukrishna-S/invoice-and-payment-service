-- What the delivery worker records about each attempt, for operators and tests.
-- last_error is a short classification ("timeout", "http 503"), never the endpoint
-- URL or response body: URLs can carry tokens and bodies are the receiver's data.
ALTER TABLE webhook_deliveries
    ADD COLUMN last_attempt_at      TIMESTAMPTZ,
    ADD COLUMN last_response_status INT,
    ADD COLUMN last_error           TEXT CHECK (char_length(last_error) <= 200),
    ADD COLUMN delivered_at         TIMESTAMPTZ,
    ADD CONSTRAINT webhook_deliveries_delivered_iff_status
        CHECK ((status = 'delivered') = (delivered_at IS NOT NULL));
