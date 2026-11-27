# Mbongo

A customer in Brazzaville taps "pay" on your app. MTN takes the money, but the answer never reaches
your server. Your app shows an error, the customer pays again, and now they've paid twice.

That kind of failure is hard to test. The operators' sandboxes rarely misbehave on demand, and home-made
mocks are too polite: they accept any token, answer "pending" for payments that don't exist, and never
lose a callback.

Mbongo ("money" in Lingala) will be a fake MTN MoMo and Airtel Money operator for Congo-Brazzaville. You
point your app at it instead of the real API, and it answers the same way, then fails the way real
operators fail: lost answers, late or repeated callbacks, payments stuck in progress, tokens that expire
early. At the end, it tells you what your app got wrong.

**It never moves real money and has nothing to do with MTN or Airtel.**

## Status

Early work. MTN's collection and payout APIs work, in their sandbox flavour and their Congo flavour, and
so do Airtel Money Congo's collections, refunds, cash-in, cash-out and payouts, all on top of books where
every movement adds up to zero. Failures on demand come in step 6.

| Step | What | State |
| --- | --- | --- |
| 1 | Server, CI, Docker image | done |
| 2 | Wallets, transactions, a clock you can move | done |
| 3 | MTN collections | done |
| 4 | MTN payouts | done |
| 5 | Airtel Money | done |
| 6 | Failures on demand | |
| 7 | A phone page to approve or ignore payments | |
| 8 | A report on your integration | |
| 9 | Checks against a public SDK, demo, write-up | |

## Run it

With Go 1.27:

```sh
go run ./cmd/mbongo
curl localhost:8080/healthz
```

With Docker:

```sh
docker build -t mbongo .
docker run --rm -p 8080:8080 mbongo
```

`PORT` or `MBONGO_ADDR` changes the address. `MBONGO_DB` is the SQLite file (`mbongo.db` by default,
`/data/mbongo.db` in the image). `MBONGO_ADMIN_TOKEN` turns on the admin API below.

## Use it in place of MTN

Point your MTN client at Mbongo instead of `sandbox.momodeveloper.mtn.com`. Then get a subscription
key, as you would from MTN's portal:

```sh
curl -X POST localhost:8080/_mbongo/mtn/subscriptions \
  -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN" -d '{"product":"collection"}'
```

**Sandbox:** create the API user and key through `/v1_0/apiuser`, exactly as with MTN. Payments are in
euros, succeed at once, and MTN's test numbers fail on purpose (`46733123451` is rejected, `46733123452`
times out).

**Congo (`mtncongo`):** payments are in francs, and the payer needs a wallet and has to say yes:

```sh
curl -X POST localhost:8080/_mbongo/mtn/api-users -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN" \
  -d '{"subscriptionKey":"...","providerCallbackHost":"shop.example","targetEnvironment":"mtncongo"}'
curl -X POST localhost:8080/_mbongo/customers -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN" \
  -d '{"operator":"mtn","phone":"242061234567","currency":"XAF","balance":"5000"}'
# after your app asks for a payment:
curl -X POST localhost:8080/_mbongo/mtn/<apiUser>/requests/<X-Reference-Id>/approve \
  -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN"
```

A payer who doesn't answer within five minutes lets the payment expire.

**Payouts** (`transfer`, `deposit`, `refund`) need a `disbursement` subscription and API user of their
own. Give both subscriptions the same `partner` (`{"product":"disbursement","partner":"my-shop"}`) so
refunds can reach your collections. In Congo, top up the account payouts are paid from first:

```sh
curl -X POST localhost:8080/_mbongo/mtn/<apiUser>/fund -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN" \
  -d '{"amount":"250000"}'
```

## Use it in place of Airtel

Point your Airtel client at Mbongo instead of `openapiuat.airtel.cg`. Create an application, as on
Airtel's portal; the answer has the client id and secret, and the PIN payouts need (1234 unless you
give one). Callbacks go to `callbackUrl`, signed when you give a `callbackKey`:

```sh
curl -X POST localhost:8080/_mbongo/airtel/apps -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN"   -d '{"name":"My Shop","callbackUrl":"https://shop.example/airtel","callbackKey":"secret"}'
curl -X POST localhost:8080/_mbongo/customers -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN"   -d '{"operator":"airtel","phone":"055123456","currency":"XAF","balance":"5000"}'
# after your app asks for a payment (POST /merchant/v1/payments/):
curl -X POST localhost:8080/_mbongo/airtel/<clientId>/payments/<transaction.id>/approve   -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN"
# before paying out (cash-in or disbursement):
curl -X POST localhost:8080/_mbongo/airtel/<clientId>/fund -H "Authorization: Bearer $MBONGO_ADMIN_TOKEN"   -d '{"amount":"250000"}'
```

Encrypt the PIN with the key from `GET /v1/rsa/encryption-keys`, as with Airtel. Phone numbers have no
242: a number sent with it is an unknown customer.

## How close to the real thing

Every behaviour says where it comes from: the operator's documentation, a public SDK, a developer's
report, what integrations do in practice, or an assumption. For MTN, the documentation is first of all
its API reference on [momodeveloper.mtn.com](https://momodeveloper.mtn.com/API-collections); for Airtel,
its Congo developer portal. See [docs/mtn.md](docs/mtn.md) and [docs/airtel.md](docs/airtel.md).
The [lepresk/momo-api](https://github.com/lepresk/momo-api) client runs against Mbongo without changes,
except for its deposits, which name a `payer` where MTN's reference wants a `payee`.

## Licence

MIT
