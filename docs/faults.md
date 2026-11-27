# Failures on demand

Real operators lose answers, time out, repeat callbacks and leave payments hanging. Mbongo does the
same when a test asks for it, through rules on its admin API (`MBONGO_ADMIN_TOKEN`).

```sh
# The next transfer to this number moves the money, then the connection drops.
curl -X POST localhost:8080/_mbongo/rules -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN" \
  -d '{"operator":"mtn","operation":"transfer","phone":"46733123453","fault":"answer_lost"}'

curl localhost:8080/_mbongo/rules -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN"            # list
curl -X DELETE localhost:8080/_mbongo/rules/3 -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN" # one
curl -X DELETE localhost:8080/_mbongo/rules -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN"   # all
```

## Rules

| Field | Meaning |
| --- | --- |
| `operator` | `mtn` or `airtel` (required) |
| `operation` | which call; empty matches every call of that operator |
| `phone` | the customer's number as the request sends it; empty matches any. Calls that send no number (token, status, balance) only match rules without a phone |
| `fault` | one of the faults below (required) |
| `times` | how many requests it applies to; 1 by default, 0 until cleared |
| `delaySeconds` | for `slow`, `callback_late` and `ambiguous` |

When several rules match, one naming a phone or an operation goes first, then the oldest.

Operations, MTN: `apiuser`, `token`, `requesttopay`, `transfer`, `deposit`, `refund`, `status`,
`balance`, `accountholder`, `callback`.
Airtel: `token`, `keys`, `collection`, `refund`, `status`, `cashout`, `cashin`, `disbursement`,
`balance`, `user`, `callback`.

## Faults

| Fault | What the client sees | What happens to the money |
| --- | --- | --- |
| `answer_lost` | the connection drops, no answer | the request is done: saved, and paid if it pays at once |
| `error_after_money` | 500 with the operator's internal error | the request is done |
| `slow` | the normal answer, after `delaySeconds` (30 by default) | the request is done |
| `rate_limited` | 429 | nothing happens |
| `maintenance` | 503 | nothing happens |
| `token_revoked` | 401, as for a token that is no longer valid | nothing happens |
| `stuck` | the normal answer; the payment stays PENDING (MTN) or TIP (Airtel) | never expires and never runs by itself; only the admin's approve or refuse decides a collection |
| `ambiguous` (Airtel) | TA with the product's `…000` code, for `delaySeconds` (180 by default) | moved at once; the status shows it afterwards |
| `callback_never` | no callback | unchanged |
| `callback_twice` | the same callback twice | unchanged |
| `callback_late` | the callback after `delaySeconds` (60 by default) | unchanged |
| `callback_before_answer` | the callback arrives before the answer to the request that caused it | unchanged |

Callback faults always go with `"operation":"callback"` (set for you when left out), and other
faults never do, so a rule meant for requests is never used up by a callback. An ambiguous Airtel
payment's callback waits until the status shows the outcome.

The 429 bodies are Azure API Management's for MTN and Airtel's usual envelope for Airtel; the 503
bodies are each operator's usual error shape (assumed: neither documents them).
