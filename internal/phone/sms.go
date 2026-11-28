package phone

import (
	"context"
	"fmt"
	"strings"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

// settled writes the SMS the operator sends the customer when a transaction ends.
func (s *Service) settled(ctx context.Context, t bank.Transaction) {
	ops, ok := s.operators[t.Operator]
	if !ok {
		return
	}
	who, ref, err := ops.Describe(ctx, t)
	if err != nil {
		s.logger.Error("sms", "transaction", t.ID, "err", err)
		return
	}
	balance := ""
	if wallets, err := s.books.Wallets(ctx, t.Operator, t.Phone); err == nil {
		for _, w := range wallets {
			if w.Currency == t.Currency {
				balance = smsAmount(w.Balance, w.Currency)
			}
		}
	}
	body := smsText(t, who, ref, balance)
	if body == "" {
		return
	}
	if err := s.store.addSMS(ctx, message{Operator: t.Operator, Number: t.Phone, Sender: senderName(t.Operator),
		Body: body, At: s.books.Clock().Now()}); err != nil {
		s.logger.Error("sms", "transaction", t.ID, "err", err)
	}
}

// smsText is empty when the customer would get no SMS: a payout that never reached them.
func smsText(t bank.Transaction, who, ref, balance string) string {
	amount := smsAmount(t.Amount, t.Currency)
	if t.Kind == bank.Payout {
		if t.Status != bank.Successful {
			return ""
		}
		return fmt.Sprintf("Vous avez reçu %s de %s. Nouveau solde: %s. Ref: %s.", amount, who, balance, ref)
	}
	switch t.Status {
	case bank.Successful:
		return fmt.Sprintf("Vous avez payé %s à %s. Frais: 0 FCFA. Nouveau solde: %s. Ref: %s.",
			amount, who, balance, ref)
	case bank.Expired:
		return fmt.Sprintf("La demande de paiement de %s de %s a expiré sans réponse.", amount, who)
	}
	switch t.Reason {
	case bank.ReasonRefused:
		return fmt.Sprintf("Vous avez refusé le paiement de %s demandé par %s.", amount, who)
	case bank.ReasonNotEnoughFunds:
		return fmt.Sprintf("Echec du paiement de %s à %s: solde insuffisant. Solde: %s.", amount, who, balance)
	}
	return fmt.Sprintf("Echec du paiement de %s à %s.", amount, who)
}

// smsAmount writes 5000 francs as "5 000 FCFA", as the operators' SMS do.
func smsAmount(minor int64, currency string) string {
	s := strings.ReplaceAll(amount(minor, currency), " ", " ")
	if currency == "XAF" || currency == "XOF" {
		return strings.TrimSuffix(s, currency) + "FCFA"
	}
	return s
}

func senderName(op bank.Operator) string {
	if op == bank.Airtel {
		return "AirtelMoney"
	}
	return "MoMo"
}
