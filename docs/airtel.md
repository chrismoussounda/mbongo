# Airtel Money Congo: what Mbongo copies, and from where

Each behaviour is tagged with its source:

- **docs**: Airtel's developer portal for Congo, [developers.airtel.cg](https://developers.airtel.cg/documentation)
  (sign-in needed), read in November 2026 and restated here in our own words.
- **practice**: what production integrations in Congo send and expect, where it differs from the docs.
- **assumed**: neither says; Mbongo's own choice, to be checked against Airtel's staging.

Phone numbers are national, without 242 (docs). A number sent with 242 is an unknown customer, so the
payment fails the way an unregistered number does.

## Calls

| Call | Answer | Source |
| --- | --- | --- |
| `POST /auth/oauth2/token` `{client_id, client_secret, grant_type}` | `{access_token, expires_in: "180", token_type: "bearer"}`; one token for every product | docs |
| same, without `client_secret` | a token | practice |
| `GET /v1/rsa/encryption-keys` | `{key_id, key, valid_upto: "dd-MM-yyyy HH:mm:ss"}`, RSA 1024 | docs; 30-day life assumed |
| `POST /merchant/v1/payments/` | waits for the customer; `data.transaction.{id, status: "SUCCESS"}`, `DP00800001006` | docs |
| `GET /standard/v1/payments/{id}` | `TIP`, `TS` (with `airtel_money_id`), `TF` or `TE`; also for cash-in and cash-out ids | docs |
| `POST /standard/v1/payments/refund` `{transaction: {airtel_money_id}}` | the whole amount back to the payer, once; a refund that failed can be tried again | docs (full refunds), assumed (retry) |
| `POST /standard/v1/cashout/` | takes the money from the customer at once, no prompt | docs (answer and description), assumed (no prompt) |
| `POST /standard/v1/cashin/` with `pin` | pays the customer from the app's wallet | docs |
| `POST /standard/v1/disbursements/` with `pin` | pays the customer; `reference_id`, `airtel_money_id`, `TS` | docs |
| `GET /standard/v1/disbursements/{id}` | the payout's status | docs |
| `GET /standard/v1/users/balance` | `{balance: "37,600.00", currency, account_status}` | docs |
| `GET /standard/v1/users/{msisdn}` | names, grade, `is_barred`, `registration.status` | docs; names are placeholders |

Cash-in's page describes the subscriber as the one "from which the payment amount deducted", while
the same page says the call credits the payee. Mbongo follows the call's description: cash-in pays the
customer, cash-out charges them.

Amounts are taken as a JSON number or a string (docs: number for collections and payouts, string for
cash-in and cash-out; practice: payouts sent as a string). XAF has no cents, so `10.5` is refused.

## Answers

Every answer is `{data, status: {code, message, result_code, response_code, success}}` (docs).

- `success` is true when there is no error, as the docs define the field. Several of the docs' examples
  show `false` beside a successful transaction; Mbongo doesn't copy that.
- A refused payment is HTTP 200 with `success: false` and the product's DP code in both `result_code`
  and `response_code` (docs: the cash-out status example carries a DP code in `result_code`).
- A broken request (missing body field, missing or wrong `X-Country`/`X-Currency`) is HTTP 400 with the
  ROUTER code (docs for the codes, assumed for the status and body). A wrong PIN is `ROUTER115`, a value
  that doesn't decrypt is `ROUTER116`, both HTTP 200 (assumed).
- A bad or expired token is 401 `{"error": "invalid_token", ...}` (assumed).

DP codes per product (docs), and the ones borrowed where a product has none (assumed):

| Case | Collection | Cash-in / cash-out | Disbursement |
| --- | --- | --- | --- |
| success | `DP00800001001` | `DP01000001001` | `DP00900001001` |
| in progress | `…006` | `…006` | `…006` |
| not enough money | `…007` | `…007` | `…007` |
| refused | `…008` | `…013` | `…013` |
| timed out | `…024` | `…014` | `…005` (failed, assumed) |
| not found | `…025` | `…005` (failed, assumed) | `…015` |
| unknown number | `…010` | `…012` | `…012` |
| id already used | `…005` (assumed) | `…011` | `…011` |
| amount below minimum | `…004` | `…004` | `…004` |

## Callbacks

Airtel posts to the one URL set for the application, not one per request (docs). Mbongo sends one POST
after a collection is approved or refused, and after a cash-out, never retried:
`{"transaction": {id, message, status_code: "TS" | "TF", airtel_money_id}}` (docs). A successful
message reads like the docs' example, "Paid XAF 1,000 to SHOP Charge XAF 0, Trans ID MP…".

When the app has a callback key, the body also carries `hash`: HMAC-SHA256 with that key, in Base64
(docs). The docs don't say which bytes are hashed; Mbongo hashes the `transaction` object exactly as
sent (assumed). No callback when a collection times out (assumed, as for MTN).

Integrations in practice often ignore callbacks and poll the status every few seconds, although the docs
advise waiting three minutes before asking about a collection.

## Not built yet

Collection v2 and disbursement v3 with message signing (`x-signature`, `x-key`), and the `TA`
(ambiguous) status, which comes with the failure rules of step 6.
