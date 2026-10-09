# Invoice & Payment Service

A Go backend for invoicing customers and collecting card payments through a PSP, with signed webhooks. It runs with PostgreSQL, a mock PSP and a small webhook receiver. See [DESIGN.md](DESIGN.md) for the reasoning and failure modes, and [openapi.yaml](openapi.yaml) for the API contract.

## Run it

You need Docker with Compose.

```sh
docker compose up --build
```

This builds everything, runs the migrations and seeds a demo business.

| Service | Address |
|---|---|
| `api` | http://localhost:8080 |
| `mockpsp` | http://127.0.0.1:8081 |
| `webhook-receiver` | http://127.0.0.1:9000 |
| `postgres` | 127.0.0.1:5432 |

Check it: `curl localhost:8080/healthz`. Wipe all data: `docker compose down -v`.

The demo API key is `sk_test_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef`, set by `docker-compose.yml` through `DEMO_API_KEY`. That variable is for local development only and must never be set in production. Without it the service generates a random key on first start and prints it once.

## Examples

These need `curl` and `jq`. Run them in order.

```bash
export API=http://localhost:8080
export KEY=sk_test_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
auth=(-H "Authorization: Bearer $KEY" -H "Content-Type: application/json")
```

**1. Create a customer, create an invoice, finalize it.** The server computes the total; `expected_total_cents` is only checked.

```bash
CUSTOMER=$(curl -s "${auth[@]}" -X POST $API/customers \
  -d '{"name": "Acme Corp", "email": "billing@acme.test"}' | jq -r .id)

INVOICE=$(curl -s "${auth[@]}" -X POST $API/invoices -d "{
  \"customer_id\": \"$CUSTOMER\", \"due_date\": \"2026-12-01\", \"expected_total_cents\": 4750,
  \"line_items\": [
    {\"description\": \"Consulting (hours)\", \"quantity\": 3, \"unit_amount_cents\": 1500},
    {\"description\": \"Setup fee\", \"quantity\": 1, \"unit_amount_cents\": 250}
  ]}" | jq -r .id)

curl -s "${auth[@]}" -X POST $API/invoices/$INVOICE/finalize | jq '{status, invoice_sequence_number, total_cents}'
```

**2. Pay it, then repeat the request.** The replay returns the same attempt and does not charge again.

```bash
curl -s "${auth[@]}" -X POST $API/invoices/$INVOICE/pay -H "Idempotency-Key: pay-001" \
  -d '{"card_token": "tok_success", "amount_cents": 4750}' | jq '{id, status}'
curl -s "${auth[@]}" -X POST $API/invoices/$INVOICE/pay -H "Idempotency-Key: pay-001" \
  -d '{"card_token": "tok_success", "amount_cents": 4750}' | jq '{id, status}'
curl -s "${auth[@]}" $API/invoices/$INVOICE | jq '{status, paid_at}'
```

**3. PSP timeout.** `tok_timeout` makes the mock PSP hold for 30 seconds. The API gives up at 5 seconds and returns `202`; the invoice stays `open`. The reconciler then asks the PSP and marks it paid.

```bash
NEW=$(curl -s "${auth[@]}" -X POST $API/invoices -d "{
  \"customer_id\": \"$CUSTOMER\", \"due_date\": \"2026-12-01\", \"expected_total_cents\": 1000,
  \"line_items\": [{\"description\": \"Slow\", \"quantity\": 1, \"unit_amount_cents\": 1000}]}" | jq -r .id)
curl -s "${auth[@]}" -X POST $API/invoices/$NEW/finalize > /dev/null

curl -s -w '\nHTTP %{http_code}\n' "${auth[@]}" -X POST $API/invoices/$NEW/pay \
  -H "Idempotency-Key: pay-slow" -d '{"card_token": "tok_timeout", "amount_cents": 1000}'
until [ "$(curl -s "${auth[@]}" $API/invoices/$NEW | jq -r .status)" = paid ]; do sleep 2; done; echo paid
```

**4. Webhook delivery.** Register the demo receiver. The secret is shown once, so pass it to the receiver, then pay an invoice and read the receiver log.

```bash
SECRET=$(curl -s "${auth[@]}" -X POST $API/webhook_endpoints \
  -d '{"url": "http://webhook-receiver:9000/hook"}' | jq -r .secret)
curl -s -X POST localhost:9000/secrets -H 'Content-Type: application/json' -d "{\"secret\": \"$SECRET\"}"

# create, finalize and pay another invoice as in examples 1 and 2, then:
docker compose logs webhook-receiver --no-log-prefix \
  | jq -c 'select(.msg | startswith("webhook received")) | {event_type, signature_verified}'
```

`tok_insufficient_funds`, `tok_card_declined` and `tok_network_error` are the other mock PSP tokens; see the table in [DESIGN.md](DESIGN.md).

## Run the tests

You need Go 1.25 and Docker.

```sh
make test    # starts Postgres through compose, runs everything with -race and coverage
make lint    # gofmt, go vet, golangci-lint
```

The integration tests use a real PostgreSQL and a real mock PSP. They are skipped with a message when `TEST_DATABASE_URL` is unset; `make test` sets it.

## Demo video

_Placeholder: link to the demo video goes here._
