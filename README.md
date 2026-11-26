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

Early work. Today it is a server with a health check and the books behind it: wallets,
payments that wait for approval or expire, and entries that always add up to zero.

| Step | What | State |
| --- | --- | --- |
| 1 | Server, CI, Docker image | done |
| 2 | Wallets, transactions, a clock you can move | done |
| 3 | MTN collections | |
| 4 | MTN payouts | |
| 5 | Airtel Money | |
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
`/data/mbongo.db` in the image).

## How close to the real thing

Every behaviour will say where it comes from: the operator's documentation, a public SDK, something seen
in a sandbox, or an assumption. When those disagree, Mbongo will let you pick.

## Licence

MIT
