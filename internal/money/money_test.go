package money_test

import (
	"errors"
	"testing"

	"github.com/chrismoussounda/mbongo/internal/money"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in, currency string
		want         int64
	}{
		{"2500", "XAF", 2500},
		{"2500.00", "XAF", 2500},
		{"10", "EUR", 1000},
		{"10.5", "EUR", 1050},
		{"10.05", "EUR", 1005},
		{"0.01", "EUR", 1},
		{"1000", "XOF", 1000},
	}
	for _, c := range cases {
		got, err := money.Parse(c.in, c.currency)
		if err != nil || got != c.want {
			t.Errorf("Parse(%q, %s) = %d, %v; want %d", c.in, c.currency, got, err, c.want)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	cases := []struct{ in, currency string }{
		{"", "XAF"},
		{"abc", "XAF"},
		{"-5", "XAF"},
		{"+5", "XAF"},
		{"2500.5", "XAF"},
		{"10.123", "EUR"},
		{"1e3", "EUR"},
		{"10.", "EUR"},
		{".5", "EUR"},
		{" 10", "EUR"},
		{"99999999999999999999", "XAF"},
	}
	for _, c := range cases {
		if got, err := money.Parse(c.in, c.currency); !errors.Is(err, money.ErrAmount) {
			t.Errorf("Parse(%q, %s) = %d, %v; want ErrAmount", c.in, c.currency, got, err)
		}
	}
	if _, err := money.Parse("10", "ZZZ"); !errors.Is(err, money.ErrCurrency) {
		t.Errorf("unknown currency: %v", err)
	}
}

func TestFormat(t *testing.T) {
	cases := []struct {
		minor    int64
		currency string
		want     string
	}{
		{2500, "XAF", "2500"},
		{1050, "EUR", "10.50"},
		{5, "EUR", "0.05"},
		{0, "EUR", "0.00"},
		{-1050, "EUR", "-10.50"},
	}
	for _, c := range cases {
		if got, err := money.Format(c.minor, c.currency); err != nil || got != c.want {
			t.Errorf("Format(%d, %s) = %q, %v; want %q", c.minor, c.currency, got, err, c.want)
		}
	}
	if _, err := money.Format(1, "ZZZ"); !errors.Is(err, money.ErrCurrency) {
		t.Errorf("unknown currency: %v", err)
	}
}
