# The phone

The phone stands in for the customer's handset. When your shop asks a customer to pay, the request
pops up on the phone: approve it, refuse it or ignore it, then check what your integration received.
The SMS show what the customer would get.

```sh
MBONGO_ADMIN_TOKEN=secret mbongo
# a customer
curl -X POST localhost:8080/_mbongo/customers -H "Authorization: Bearer secret" \
  -d '{"operator":"mtn","phone":"242061234567","currency":"XAF","balance":"50000"}'
# a second SIM in the same phone
curl -X POST localhost:8080/_mbongo/customers -H "Authorization: Bearer secret" \
  -d '{"operator":"airtel","phone":"055000001","currency":"XAF","balance":"20000","sameDeviceAs":"242061234567"}'
```

Every customer has a phone. Open `http://localhost:8080/_mbongo/phone/` and pick one.
`/_mbongo/phone/{mtn|airtel}/{number}` opens the phone holding that number.

## Numbers you don't create

With `MBONGO_AUTO_CUSTOMERS=true`, a number an MTN Congo or Airtel call uses (a payment, a payout, a
lookup) becomes a customer the first time, with 100 000 XAF (`MBONGO_AUTO_BALANCE` changes it), and
gets a phone, as long as it is that operator's line: `24206` and seven digits for MTN, `05` or `04` and
seven digits for Airtel. Any other number, and every unknown number without the setting, fails the way
the operators fail them (`PAYER_NOT_FOUND`, `Invalid mobile number`). MTN's sandbox environment takes
any number either way.

The phones list pops a browser notification for every payment asked of any number, once allowed with
its alerts button; clicking it opens that phone.

## What the phone does

- **Home screen.** Status bar with the time, the signal of each SIM and the battery, and three apps:
  Messages, and one wallet app per SIM (MTN MoMo, Airtel Money). Badges count unread SMS and payments
  waiting.
- **Pop-up.** A payment a shop asks for pops up over whatever app is open, with the SIM it came on,
  who asks, how much, the shop's message (MTN `payerMessage`, Airtel `reference`) and the time left.
  The phone shakes. With the tab in the background, a browser notification shows too, once allowed
  with the Alerts button.
- **Approve** moves the money: MTN `SUCCESSFUL`, Airtel `TS`.
- **Refuse** fails the payment: MTN `FAILED` with `APPROVAL_REJECTED`, Airtel `TF`.
- **Ignore** closes the pop-up only. The payment waits under Approvals in the wallet app until it
  expires, and Open brings it back.
- **Wallet apps** show the balance, Approvals and the history.
- **Messages** has one thread per SIM. An SMS arrives on every money move: a payment made, refused,
  failed for lack of money or expired, and money received. A payout that never reached the customer
  sends nothing.
- Everything updates by itself (server-sent events). Without JavaScript, reloading shows the same.

Which payments pop up: MTN requests to pay and Airtel collections. Payouts, deposits, refunds and
Airtel cash-in or cash-out run without the customer and only show in the history and the SMS. A
payment held by a `stuck` rule pops up with no time limit.

## SMS

| Event | Text |
| --- | --- |
| Payment made | `Vous avez payé 2 500 FCFA à Boutique Mama. Frais: 0 FCFA. Nouveau solde: 7 500 FCFA. Ref: 2852348891.` |
| Refused | `Vous avez refusé le paiement de 100 FCFA demandé par Boutique Mama.` |
| Not enough money | `Echec du paiement de 99 999 FCFA à Boutique Mama: solde insuffisant. Solde: 7 500 FCFA.` |
| Expired | `La demande de paiement de 300 FCFA de Boutique Mama a expiré sans réponse.` |
| Money received | `Vous avez reçu 1 000 FCFA de Boutique Mama. Nouveau solde: 8 500 FCFA. Ref: 2852348891.` |

The sender is `MoMo` or `AirtelMoney`. The reference is MTN's `financialTransactionId` or Airtel's
`airtel_money_id`, the same the shop gets.

The phone pages are open to anyone who can reach Mbongo, like the rest of a test server; only the
admin API needs `MBONGO_ADMIN_TOKEN`. Forms refuse posts from other sites.
