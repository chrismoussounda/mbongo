package phone

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/money"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/*
var static embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"clock": func(t time.Time) string { return t.In(congo).Format("15:04:05") },
}).ParseFS(templateFiles, "templates/*.html"))

// congo is Brazzaville's time, UTC+1 all year, so the page needs no time zone database.
var congo = time.FixedZone("WAT", 60*60)

// appNames are the screens a phone can show.
var appNames = map[string]string{"home": "home", "messages": "messages", "sim1": "sim1", "sim2": "sim2"}

type phoneLink struct {
	URL  string
	SIMs []string
}

type indexView struct {
	Phones []phoneLink
}

type missingView struct {
	Operator, Number string
}

type notice struct {
	Text  string
	Error bool
}

type wallet struct {
	Currency, Balance string
}

type prompt struct {
	ID, Payee, Message, Amount, Action, SIM string
	ExpiresAt                               time.Time
	// NoLimit is a payment that never expires; otherwise SecondsLeft drives the countdown.
	NoLimit     bool
	SecondsLeft int
}

type line struct {
	When, What, Amount string
	// State is a short key for styling, Status the words shown.
	State, Status string
	Incoming      bool
}

type sms struct {
	When, Body string
	Unread     bool
}

type simView struct {
	Slot             int
	App              string
	Key              string
	Operator, Number string
	Sender           string
	Wallets          []wallet
	History          []line
	Approvals        []prompt
	Messages         []sms
	Unread           int
	Last             string
}

type phoneView struct {
	URL      string
	Clock    string
	App      string
	SIMs     []simView
	Current  *simView
	Prompt   *prompt
	Waiting  int
	Unread   int
	Notice   notice
	Messages bool
}

func (s *Service) indexView(ctx context.Context) (indexView, error) {
	var v indexView
	phones, err := s.store.phones(ctx)
	if err != nil {
		return v, err
	}
	for _, p := range phones {
		link := phoneLink{URL: phoneURL(p.ID)}
		for _, sim := range p.SIMs {
			link.SIMs = append(link.SIMs, operatorName(sim.Operator)+" "+sim.Number)
		}
		v.Phones = append(v.Phones, link)
	}
	placed := map[SIM]bool{}
	for _, p := range phones {
		for _, sim := range p.SIMs {
			placed[SIM{Operator: sim.Operator, Number: sim.Number}] = true
		}
	}
	customers, err := s.books.Customers(ctx)
	if err != nil {
		return v, err
	}
	for _, c := range customers {
		key := SIM{Operator: c.Operator, Number: c.Name}
		if placed[key] {
			continue
		}
		placed[key] = true
		v.Phones = append(v.Phones, phoneLink{URL: base + url.PathEscape(string(c.Operator)) + "/" + url.PathEscape(c.Name),
			SIMs: []string{operatorName(c.Operator) + " " + c.Name}})
	}
	return v, nil
}

func (s *Service) view(ctx context.Context, id int64, sims []SIM, app, slot string) (phoneView, error) {
	now := s.books.Clock().Now()
	v := phoneView{URL: phoneURL(id), Clock: now.In(congo).Format("15:04"), App: "home"}
	if name, ok := appNames[app]; ok {
		v.App = name
	}
	var all []prompt
	for _, sim := range sims {
		sv, prompts, err := s.simView(ctx, sim, now)
		if err != nil {
			return v, err
		}
		v.Unread += sv.Unread
		v.SIMs = append(v.SIMs, sv)
		all = append(all, prompts...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ExpiresAt.Before(all[j].ExpiresAt) })
	for i := range all {
		all[i].Action = v.URL + "/prompts/" + url.PathEscape(all[i].ID)
		if s.isIgnored(all[i].ID) {
			continue
		}
		if v.Prompt != nil {
			v.Waiting++
			continue
		}
		p := all[i]
		v.Prompt = &p
	}
	for i := range v.SIMs {
		sv := &v.SIMs[i]
		for j := range sv.Approvals {
			sv.Approvals[j].Action = v.URL + "/prompts/" + url.PathEscape(sv.Approvals[j].ID)
		}
		if v.App == sv.App {
			v.Current = sv
		}
	}
	if v.App == "messages" {
		v.Messages = true
		for i := range v.SIMs {
			if strconv.Itoa(v.SIMs[i].Slot) != slot {
				continue
			}
			v.Current = &v.SIMs[i]
			sim := sims[i]
			if err := s.store.markRead(ctx, sim.Operator, sim.Number); err != nil {
				return v, err
			}
		}
	}
	return v, nil
}

func (s *Service) simView(ctx context.Context, sim SIM, now time.Time) (simView, []prompt, error) {
	sv := simView{Slot: sim.Slot, App: "sim" + strconv.Itoa(sim.Slot), Operator: operatorName(sim.Operator),
		Number: sim.Number, Sender: senderName(sim.Operator), Key: string(sim.Operator)}
	wallets, err := s.books.Wallets(ctx, sim.Operator, sim.Number)
	if err != nil {
		return sv, nil, err
	}
	for _, a := range wallets {
		sv.Wallets = append(sv.Wallets, wallet{Currency: a.Currency, Balance: amount(a.Balance, a.Currency)})
	}
	waiting, err := s.operators[sim.Operator].Prompts(ctx, sim.Number)
	if err != nil {
		return sv, nil, err
	}
	var prompts []prompt
	for _, p := range waiting {
		pv := prompt{ID: p.ID, Payee: p.Payee, Message: p.Message, Amount: amount(p.Amount, p.Currency),
			ExpiresAt: p.ExpiresAt, SIM: "SIM " + strconv.Itoa(sim.Slot) + " · " + operatorName(sim.Operator)}
		if left := p.ExpiresAt.Sub(now); left > 24*time.Hour {
			pv.NoLimit = true
		} else {
			pv.SecondsLeft = max(1, int(left.Round(time.Second).Seconds()))
		}
		prompts = append(prompts, pv)
		sv.Approvals = append(sv.Approvals, pv)
	}
	history, err := s.books.History(ctx, sim.Operator, sim.Number, historyLen)
	if err != nil {
		return sv, nil, err
	}
	for _, t := range history {
		l := line{When: t.CreatedAt.In(congo).Format("02 Jan 15:04"), Amount: amount(t.Amount, t.Currency)}
		l.State, l.Status = state(t)
		if t.Kind == bank.Payout {
			l.What, l.Incoming = "Received", true
		} else {
			l.What = "Payment"
		}
		sv.History = append(sv.History, l)
	}
	inbox, err := s.store.inbox(ctx, sim.Operator, sim.Number, inboxLen)
	if err != nil {
		return sv, nil, err
	}
	for i := len(inbox) - 1; i >= 0; i-- {
		m := inbox[i]
		sv.Messages = append(sv.Messages, sms{When: m.At.In(congo).Format("02 Jan 15:04"), Body: m.Body, Unread: !m.Read})
		if !m.Read {
			sv.Unread++
		}
	}
	if len(inbox) > 0 {
		sv.Last = inbox[0].Body
	}
	return sv, prompts, nil
}

func state(t bank.Transaction) (key, words string) {
	switch t.Status {
	case bank.Successful:
		return "done", "done"
	case bank.Pending:
		return "waiting", "waiting"
	case bank.Expired:
		return "expired", "expired"
	}
	switch t.Reason {
	case bank.ReasonRefused:
		return "refused", "refused"
	case bank.ReasonNotEnoughFunds:
		return "funds", "not enough money"
	}
	return "failed", "failed"
}

func operatorName(op bank.Operator) string {
	switch op {
	case bank.MTN:
		return "MTN MoMo"
	case bank.Airtel:
		return "Airtel Money"
	}
	return string(op)
}

func phoneURL(id int64) string {
	return base + "p/" + strconv.FormatInt(id, 10)
}

// amount writes 1250000 francs as "1 250 000 XAF", with no-break spaces.
func amount(minor int64, currency string) string {
	s, err := money.Format(minor, currency)
	if err != nil {
		return strconv.FormatInt(minor, 10) + " " + currency
	}
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(c)
	}
	if hasFrac {
		b.WriteString("." + frac)
	}
	return sign + b.String() + " " + currency
}

// render writes the whole page or nothing, so a template error is a clean 500.
func (s *Service) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		s.internal(w, err)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy",
		"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
