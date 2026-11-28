# The report

Mbongo records every call your app makes, every callback it sends and what your endpoint answered,
then looks for the mistakes that lose money or move it twice. The report is per client: an MTN API
user, or an Airtel app.

- **Page:** `/_mbongo/report/` lists the clients of the current run with their errors and warnings;
  each one opens a page with its findings, the calls behind each finding, and its whole traffic. Like
  the phone pages, it is open to anyone who can reach Mbongo.
- **JSON:** `GET /_mbongo/report` on the admin API (`MBONGO_ADMIN_TOKEN`). It always answers 200,
  whatever it finds, so a test reads the counts:

```sh
curl -s localhost:8080/_mbongo/report -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN" | jq -e '.errors == 0'
```

`?run=2` reads an older run, `?operator=mtn` and `?client=<API user or client_id>` keep one client.
Each finding has a `check`, a `group`, a `severity` (`error` or `warning`), the `reference` it is about,
a `message`, and the ids of the `calls` behind it.

## Runs

A run is a stretch of traffic. The first one starts with Mbongo and goes on across restarts.
`POST /_mbongo/report/runs` starts the next one; what follows is recorded in it, and older runs stay
readable. `GET /_mbongo/report/runs` lists them. Start a run before each test suite so earlier
experiments don't show up in its report.

## Checks

An **error** loses money, moves it twice, or leaves it unaccounted for. A **warning** is a habit that
will cause one sooner or later.

### Money moved twice

| Check | Severity | When |
| --- | --- | --- |
| `order_paid_twice` | error | Two payments with the same order (`externalId`, Airtel `reference`) both succeeded |
| `payout_twice` | warning | Two equal payouts to one number within 10 minutes both succeeded |
| `lost_answer_new_reference` | error | A payment got no answer or a 5xx, then the same order (or number and amount) was sent under a new reference without a status call first. Retry with the same reference, or ask the status |
| `failed_retried_same_reference` | warning | A payment the app knew had failed was sent again with its old reference; a new attempt needs a new one |

### Trust and follow-up

| Check | Severity | When |
| --- | --- | --- |
| `callback_trusted` | warning | A callback said a payment succeeded and no status call followed |
| `money_unconfirmed` | error | A payment succeeded and the app never learned it: it never asked, or stopped asking while it was pending, and no callback reached it |
| `failure_unseen` | warning | The same for a payment that failed |

### Refunds

| Check | Severity | When |
| --- | --- | --- |
| `refund_of_refund` | error | A refund named a refund |
| `refund_of_payout` | error | A refund named a payout |
| `over_refund` | error | Refunds asked back more than was paid, counting those that succeeded or are on their way |
| `refund_while_pending` | error | A second refund of a payment was sent while the first one's outcome was still unknown to the app |

A failed refund resent with its old reference shows as `failed_retried_same_reference`.

### API hygiene

| Check | Severity | When |
| --- | --- | --- |
| `token_per_call` | warning | Three or more new tokens asked for while the last one had more than half its life left (MTN 1 h, Airtel 3 min), and no call had been refused for its token |
| `polling_too_fast` | warning | The status of one payment asked three times or more less than 5 s apart |
| `rate_limit_ignored` | warning | A call sent less than 1 s after a 429 |
| `callback_failing` | error | The callback endpoint answered other than 2xx, or could not be reached; a callback is sent once and never retried |
| `callback_slow` | warning | The callback endpoint took more than 3 s to answer |

Calls that a rule made fail say so on the page (`rule: answer_lost`), so you can tell Mbongo's doing
from your app's.

## What it cannot see

Mbongo only sees your app from the outside. It cannot see your own books: whether an order was
delivered, marked paid, or paid out twice from your side is for your own tests. `callback_trusted`
means no status call followed; it cannot tell whether your app acted on the callback. Payments still
pending are not judged; read the report again once they have ended.

## Sources

The checks follow what operators and payment guides ask of integrations: check the status on the
server before fulfilling an order, process a payment once whatever the callbacks say, poll after
20 to 25 seconds and back off, never send a second refund while one is pending, retry a failed refund
under a new id, and reuse tokens until they expire (PhonePe's UAT checklist; OWASP's payment gateway
cheat sheet; MTN's callback and integration guides). The thresholds (10 minutes, 5 s, 3 s, three
tokens) are Mbongo's own.
