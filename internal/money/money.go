// Package money converts amounts between the operators' text form ("10.50") and whole
// numbers of the currency's smallest unit (1050 cents), so no amount is ever a float.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrCurrency = errors.New("money: unknown currency")
	ErrAmount   = errors.New("money: invalid amount")
)

// decimals per currency, from ISO 4217. Central African and West African francs have none.
var decimals = map[string]int{
	"XAF": 0,
	"XOF": 0,
	"EUR": 2,
}

func Known(currency string) bool {
	_, ok := decimals[currency]
	return ok
}

// Trailing zeros past the currency's decimals are accepted ("2500.00" francs).
func Parse(s, currency string) (int64, error) {
	d, ok := decimals[currency]
	if !ok {
		return 0, ErrCurrency
	}
	whole, frac, hasPoint := strings.Cut(s, ".")
	if !digits(whole) || (hasPoint && !digits(frac)) {
		return 0, fmt.Errorf("%w: %q", ErrAmount, s)
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > d {
		return 0, fmt.Errorf("%w: %q has more than %d decimals for %s", ErrAmount, s, d, currency)
	}
	n, err := strconv.ParseInt(whole+frac+strings.Repeat("0", d-len(frac)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrAmount, s)
	}
	return n, nil
}

func Format(minor int64, currency string) (string, error) {
	d, ok := decimals[currency]
	if !ok {
		return "", ErrCurrency
	}
	if d == 0 {
		return strconv.FormatInt(minor, 10), nil
	}
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	unit := int64(math.Pow10(d))
	return fmt.Sprintf("%s%d.%0*d", sign, minor/unit, d, minor%unit), nil
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
