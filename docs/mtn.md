# MTN MoMo: what Mbongo copies, and from where

Each behaviour is tagged with its source:

- **sandbox**: what MTN's own sandbox (`sandbox.momodeveloper.mtn.com`) answered when the same calls
  were sent to it and to Mbongo.
- **docs**: MTN's own API reference ([API Sandbox](https://momodeveloper.mtn.com/API-collections): Collection,
  Disbursements and Sandbox User Provisioning, with each operation's headers, bodies and examples) and pages
  ([callbacks](https://momoapi.mtn.com/content/html_widgets/lflwl.html),
  [FAQ](https://momodeveloper.mtn.com/content/html_widgets/qoftb.html),
  [failure reasons](https://momodeveloper.mtn.com/content/html_widgets/4p5s8.html),
  [request to pay](https://momoapi.mtn.com/content/html_widgets/i5xpi.html),
  [API users](https://momoapi.mtn.com/content/html_widgets/wabm9.html)).
- **sdk**: [lepresk/momo-api](https://github.com/lepresk/momo-api), used against MTN Congo in production,
  for what only Congo shows.
- **report**: a developer's write-up or forum post, not confirmed by MTN.
- **assumed**: our best guess; tell us if the real operator differs.

## Environments

| | `sandbox` | `mtncongo` |
| --- | --- | --- |
| Currency | EUR only (sandbox) | XAF |
| Payer | any number, with enough money; a few test numbers fail on purpose (sandbox) | must have a wallet, and approve (assumed) |
| Failure reason in status | bare string, such as `"APPROVAL_REJECTED"` (sandbox) | bare string (sdk) |
| Balance and account holder check | refused (sandbox) | answered (sdk) |
| API users | created through the API (docs, sandbox) | created by Mbongo's admin API, like MTN's partner portal (assumed) |

Sandbox test numbers (sandbox): `46733123450` fails with `INTERNAL_PROCESSING_ERROR`, `46733123451` is
rejected with `APPROVAL_REJECTED`, `46733123452` fails with `EXPIRED` at once, `46733123454` stays
pending. Any other number succeeds at once in Mbongo. MTN's sandbox left a payment to another number
pending until it expired, and a transfer pending; Mbongo approves them so the happy path stays testable.

## Calls

| Call | Answer | Source |
| --- | --- | --- |
| `POST /v1_0/apiuser` with `X-Reference-Id` (UUID v4) and `{"providerCallbackHost"}` | 201, empty | docs, sandbox |
| `POST /v1_0/apiuser/{id}/apikey` | 201 `{"apiKey"}` | docs, sandbox |
| `GET /v1_0/apiuser/{id}` | 200 `{"providerCallbackHost","targetEnvironment"}` | docs, sandbox |
| `POST /collection/token/` with Basic auth and subscription key; `X-Target-Environment` not needed | 200 `{"access_token","token_type":"access_token","expires_in":3600}` | sandbox |
| `POST /collection/v1_0/requesttopay` | 202, empty body | docs, sandbox |
| `GET /collection/v1_0/requesttopay/{referenceId}` | 200 status body | docs, sandbox |
| `GET /collection/v1_0/account/balance` | sandbox: 404 `RESOURCE_NOT_FOUND`; Congo: 200 `{"availableBalance","currency"}` | sandbox, sdk |
| `GET /collection/v1_0/accountholder/msisdn/{msisdn}/active` | sandbox: 500 `NOT_ALLOWED_TARGET_ENVIRONMENT`; Congo: 200 `{"result": bool}` | sandbox, sdk |

Status body: `amount` (text), `currency`, `externalId`, `payer {partyIdType, partyId}`, `payerMessage`,
`payeeNote`, `status` (`PENDING`, `SUCCESSFUL`, `FAILED`), `financialTransactionId` only once
successful, `reason` only when failed (docs, sandbox).

## Errors

Error bodies are `{"code", "message"}` (docs, sandbox).

| Case | Answer | Source |
| --- | --- | --- |
| Missing or wrong subscription key | 401, Azure API Management body `{"statusCode":401,"message":...}` | assumed |
| Wrong API user or key on the token call | 500 `{"error":"login_failed"}` | sandbox |
| Missing, expired or other-product token | 401 `{"code":"...","message":...}` | assumed |
| `X-Target-Environment` not the user's | 500 `NOT_ALLOWED_TARGET_ENVIRONMENT` | docs, sandbox |
| `X-Reference-Id` not a UUID | 400 `INVALID_REFERENCE_ID` | assumed |
| `X-Reference-Id` reused | 409 `RESOURCE_ALREADY_EXIST` | docs, sandbox |
| Unknown reference on status | 404 `RESOURCE_NOT_FOUND` | docs, sandbox |
| Callback host not the user's `providerCallbackHost` | 400 `INVALID_CALLBACK_URL_HOST` | docs (rule), assumed (status) |
| Wrong currency | 500 `INVALID_CURRENCY` | docs, sandbox |
| Payer or payee missing or not an MSISDN | 400, empty body | sandbox |
| Bad amount or decimals, message over 160 characters | 400 `VALIDATION_ERROR` | docs (code), assumed (limits) |

Failure reasons in a FAILED status: `PAYER_NOT_FOUND`, `NOT_ENOUGH_FUNDS` (Congo:
`LOW_BALANCE_OR_PAYEE_LIMIT_REACHED_OR_NOT_ALLOWED`, sdk), `APPROVAL_REJECTED`, `EXPIRED`,
`INTERNAL_PROCESSING_ERROR` (docs, sandbox).

## Payouts (disbursement)

A separate product, with its own subscription key, API user and token (docs, sandbox). Subscriptions
created with the same `partner` belong to one business, as on MTN's partner portal, so its payouts can
refund its collections (assumed).

| Call | Answer | Source |
| --- | --- | --- |
| `POST /disbursement/token/` | as for collection | docs, sandbox |
| `POST /disbursement/v1_0/transfer` with `payee` | 202, then paid at once | docs, sandbox (call), assumed (timing) |
| `POST /disbursement/v1_0/deposit` with `payee` | 202, then paid at once | docs, sandbox |
| `POST /disbursement/v1_0/refund` with `referenceIdToRefund` and no `payee` | 202, paid back to the original payer | docs (call), assumed (who is paid) |
| `GET /disbursement/v1_0/{transfer,deposit,refund}/{referenceId}` | 200 status body with `payee` | docs, sandbox |
| `GET /disbursement/v1_0/account/balance` | sandbox: 500 `NOT_ALLOWED`; Congo: as for collection | sandbox, sdk |

- Every status body carries `payerMessage` and `payeeNote` (docs, sandbox). Not built yet: `v2_0`
  deposit and refund, which MTN lists with the same body (docs).
- Nobody approves a payout. It fails with the same low-balance reason as a collection when the
  account is short (assumed for Congo), and with `PAYEE_NOT_FOUND` for an unknown number in Congo.
- Sandbox payout accounts start with plenty of euros; Congo ones start empty and are topped up with
  `POST /_mbongo/mtn/{apiUser}/fund` (assumed: MTN funds them outside the API).
- The sandbox test numbers fail or stay pending as for collections; a payee can't reject (sandbox).
- A deposit names the `payee`; one that names a `payer` gets 400 with an empty body (docs, sandbox).
- A refund needs a successful request to pay of the same partner and environment; otherwise it gets
  500 `TRANSACTION_NOT_FOUND` (sandbox; partner and environment assumed). All refunds of a payment
  together can't pass the amount paid, or the next one gets 400 `NOT_ALLOWED` (assumed).

## Callbacks

Sent once, never retried, only when the request carried `X-Callback-Url` (docs). The body is the
status body (docs: "similar to the response returned by the status check"). MTN's pages say both PUT
and POST; one developer saw no signature and a second delivery three minutes later (report).
Mbongo sends one unsigned PUT for now; repeats and other methods come with the trouble rules.

The approval window is five minutes (docs).
