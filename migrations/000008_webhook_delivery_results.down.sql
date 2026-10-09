ALTER TABLE webhook_deliveries
    DROP CONSTRAINT webhook_deliveries_delivered_iff_status,
    DROP COLUMN delivered_at,
    DROP COLUMN last_error,
    DROP COLUMN last_response_status,
    DROP COLUMN last_attempt_at;
