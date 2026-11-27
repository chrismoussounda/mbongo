# MTN MoMo Collection: what Mbongo copies, and from where

Each behaviour is tagged with its source:

- **docs**: MTN's own pages ([callbacks](https://momoapi.mtn.com/content/html_widgets/lflwl.html),
  [FAQ](https://momodeveloper.mtn.com/content/html_widgets/qoftb.html),
  [failure reasons](https://momodeveloper.mtn.com/content/html_widgets/4p5s8.html),
  [request to pay](https://momoapi.mtn.com/content/html_widgets/i5xpi.html),
  [API users](https://momoapi.mtn.com/content/html_widgets/wabm9.html)).
- **sdk**: [lepresk/momo-api](https://github.com/lepresk/momo-api) (used against MTN Congo in production)
  and its test fixtures.
- **report**: a developer's write-up or forum post, not confirmed by MTN.
- **assumed**: our best guess; tell us if the real operator differs.

## Environments

| | `sandbox` | `mtncongo` |
| --- | --- | --- |
| Currency | EUR only (sdk, report) | XAF |
| Payer | any number; a few test numbers fail on purpose (sdk) | must have a wallet, and approve (assumed) |
| Failure reason in status | object `{code, message}` (sdk) | bare string (sdk: "since January 2024") |
| API users | created through the API (docs) | created by Mbongo's admin API, like MTN's partner portal (assumed) |

Sandbox test numbers (sdk): `46733123450` fails, `46733123451` is rejected, `46733123452` times out,
`46733123454` stays pending. Any other number succeeds at once.

## Calls

| Call | Answer | Source |
| --- | --- | --- |
| `POST /v1_0/apiuser` with `X-Reference-Id` (UUID v4) and `{"providerCallbackHost"}` | 201, empty | docs |
| `POST /v1_0/apiuser/{id}/apikey` | 201 `{"apiKey"}` | docs, sdk |
| `GET /v1_0/apiuser/{id}` | 200 `{"providerCallbackHost","targetEnvironment"}` | docs |
| `POST /collection/token/` with Basic auth, subscription key, `X-Target-Environment` | 200 `{"access_token","token_type":"access_token","expires_in":3600}` | sdk, report |
| `POST /collection/v1_0/requesttopay` | 202, empty body | docs, report |
| `GET /collection/v1_0/requesttopay/{referenceId}` | 200 status body | docs, sdk |
| `GET /collection/v1_0/account/balance` | 200 `{"availableBalance","currency"}` | sdk |
| `GET /collection/v1_0/accountholder/msisdn/{msisdn}/active` | 200 `{"result": bool}` | sdk |

Status body: `amount` (text), `currency`, `externalId`, `payer {partyIdType, partyId}`, `payerMessage`,
`payeeNote`, `status` (`PENDING`, `SUCCESSFUL`, `FAILED`), `financialTransactionId` only once
successful, `reason` only when failed (docs, sdk, report).

## Errors

Error bodies are `{"code", "message"}` (sdk, report).

| Case | Answer | Source |
| --- | --- | --- |
| Missing or wrong subscription key | 401, Azure API Management body `{"statusCode":401,"message":...}` | assumed |
| Wrong API user or key on the token call | 401 `{"error":"login_failed"}` | assumed (sdk models a 401 with `error`) |
| Missing, expired or other-product token | 401 `{"code":"...","message":...}` | assumed |
| `X-Target-Environment` missing or not the user's | 403 `NOT_ALLOWED_TARGET_ENVIRONMENT` | docs (code), assumed (status) |
| `X-Reference-Id` not a UUID | 400 `INVALID_REFERENCE_ID` | sdk (400), assumed (code) |
| `X-Reference-Id` reused | 409 `RESOURCE_ALREADY_EXIST` | docs, report |
| Unknown reference on status | 404 `RESOURCE_NOT_FOUND` | sdk |
| Callback host not the user's `providerCallbackHost` | 400 `INVALID_CALLBACK_URL_HOST` | docs (rule), assumed (status) |
| Wrong currency | 400 `INVALID_CURRENCY` | docs (code), sdk (EUR in sandbox) |
| Bad amount, decimals, payer, or message over 160 characters | 400 `VALIDATION_ERROR` | docs (code), assumed (limits) |

Failure reasons in a FAILED status: `PAYER_NOT_FOUND`, `NOT_ENOUGH_FUNDS` (Congo:
`LOW_BALANCE_OR_PAYEE_LIMIT_REACHED_OR_NOT_ALLOWED`), `APPROVAL_REJECTED`, `EXPIRED`,
`INTERNAL_PROCESSING_ERROR` (docs, sdk).

## Callbacks

Sent once, never retried, only when the request carried `X-Callback-Url` (docs). The body is the
status body (docs: "similar to the response returned by the status check"). MTN's pages say both PUT
and POST; one developer saw no signature and a second delivery three minutes later (report).
Mbongo sends one unsigned PUT for now; repeats and other methods come with the trouble rules.

The approval window is five minutes (docs).
