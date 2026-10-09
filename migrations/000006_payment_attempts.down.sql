ALTER TABLE invoice_transitions DROP COLUMN attempt_id;
ALTER TABLE invoices DROP CONSTRAINT invoices_latest_attempt_fk;
ALTER TABLE invoices DROP COLUMN latest_payment_attempt_id;
DROP TABLE invoice_payment_idempotency_keys;
DROP TABLE invoice_payment_attempts;
