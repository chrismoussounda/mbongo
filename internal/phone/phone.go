// Package phone is the customer's side of a payment: a phone with one or two SIMs where
// requests to pay pop up, to approve, refuse or leave until they expire, and
// where the operators' SMS arrive.
package phone

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

const (
	base       = "/_mbongo/phone/"
	historyLen = 20
	inboxLen   = 50
	heartbeat  = 15 * time.Second
)

// Operator is what the phone needs from an operator: what waits on a number or on any, the
// payer's answer, and who a transaction is with and its reference, for the SMS.
type Operator interface {
	Prompts(ctx context.Context, phone string) ([]bank.Prompt, error)
	Waiting(ctx context.Context) ([]bank.Prompt, error)
	Answer(ctx context.Context, transactionID string, approve bool) error
	Describe(ctx context.Context, t bank.Transaction) (counterparty, ref string, err error)
}

type Service struct {
	books     *bank.Bank
	operators map[bank.Operator]Operator
	logger    *slog.Logger
	store     *store

	mu      sync.Mutex
	ignored map[string]time.Time
}

// New also starts writing the operators' SMS as transactions end.
func New(ctx context.Context, books *bank.Bank, operators map[bank.Operator]Operator, logger *slog.Logger) (*Service, error) {
	st, err := newStore(ctx, books)
	if err != nil {
		return nil, err
	}
	s := &Service{books: books, operators: operators, logger: logger, store: st, ignored: map[string]time.Time{}}
	books.OnSettled(s.settled)
	return s, nil
}

var (
	ErrFull          = errFull
	ErrUnknownNumber = errUnknownNumber
	ErrAmbiguous     = errAmbiguous
)

// OpenInPhoneOf opens a new customer's wallet and puts the SIM into the phone holding number,
// together or not at all.
func (s *Service) OpenInPhoneOf(ctx context.Context, op bank.Operator, newNumber, currency string, balance int64,
	number string) (bank.Account, error) {
	return s.books.OpenCustomerWith(ctx, op, newNumber, currency, balance, func(tx *sql.Tx) error {
		return join(ctx, tx, SIM{Operator: op, Number: newNumber}, number)
	})
}

type handler struct {
	*Service
	// stopping ends the event streams, which would otherwise hold a shutdown open.
	stopping <-chan struct{}
}

// Routes serves the phones. Open pages stop listening for changes when ctx ends.
func (s *Service) Routes(ctx context.Context, mux *http.ServeMux) {
	h := &handler{Service: s, stopping: ctx.Done()}
	csrf := http.NewCrossOriginProtection()
	for _, file := range []string{"phone.css", "phone.js"} {
		mux.HandleFunc("GET "+base+"static/"+file, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, static, "static/"+file)
		})
	}
	mux.HandleFunc("GET "+base+"{$}", h.index)
	mux.HandleFunc("GET "+base+"events", h.feed)
	mux.HandleFunc("GET "+base+"{operator}/{number}", h.simPhone)
	mux.HandleFunc("GET "+base+"p/{id}", h.page)
	mux.HandleFunc("GET "+base+"p/{id}/events", h.events)
	mux.Handle("POST "+base+"p/{id}/prompts/{tx}", csrf.Handler(http.HandlerFunc(h.answer)))
}

func (h *handler) index(w http.ResponseWriter, r *http.Request) {
	v, err := h.indexView(r.Context())
	if err != nil {
		h.internal(w, err)
		return
	}
	h.render(w, http.StatusOK, "index", v)
}

// simPhone opens the phone holding a number, giving the number a phone of its own if needed.
func (h *handler) simPhone(w http.ResponseWriter, r *http.Request) {
	op := bank.Operator(r.PathValue("operator"))
	number := r.PathValue("number")
	if _, known := h.operators[op]; !known {
		http.NotFound(w, r)
		return
	}
	wallets, err := h.books.Wallets(r.Context(), op, number)
	if err != nil {
		h.internal(w, err)
		return
	}
	if len(wallets) == 0 {
		h.render(w, http.StatusNotFound, "missing", missingView{Operator: operatorName(op), Number: number})
		return
	}
	id, err := h.store.phoneOf(r.Context(), op, number)
	if err != nil {
		h.internal(w, err)
		return
	}
	//nolint:gosec // the target is a phone page: a number from the database
	http.Redirect(w, r, phoneURL(id), http.StatusSeeOther)
}

// device reads the phone from the path, answering 404 for one that does not exist.
func (h *handler) device(w http.ResponseWriter, r *http.Request) (int64, []SIM, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, nil, false
	}
	sims, err := h.store.sims(r.Context(), id)
	if err != nil {
		h.internal(w, err)
		return 0, nil, false
	}
	if len(sims) == 0 {
		http.NotFound(w, r)
		return 0, nil, false
	}
	return id, sims, true
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	id, sims, ok := h.device(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	v, err := h.view(r.Context(), id, sims, q.Get("app"), q.Get("sim"))
	if err != nil {
		h.internal(w, err)
		return
	}
	v.Notice = notices[q.Get("notice")]
	h.render(w, http.StatusOK, "phone", v)
}

// notices are the messages a decision leaves on the page after its redirect.
var notices = map[string]notice{
	"approved": {Text: "Payment approved."},
	"refused":  {Text: "Payment refused."},
	"ignored":  {Text: "Closed. The payment waits under Approvals until it expires."},
	"decided":  {Text: "This payment is no longer waiting for you.", Error: true},
}

func (h *handler) answer(w http.ResponseWriter, r *http.Request) {
	id, sims, ok := h.device(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	txID := r.PathValue("tx")
	t, err := h.books.Transaction(r.Context(), txID)
	if err != nil && !errors.Is(err, bank.ErrNotFound) {
		h.internal(w, err)
		return
	}
	sim, held := simOf(sims, t)
	if errors.Is(err, bank.ErrNotFound) || !held || t.Kind != bank.Collection {
		http.NotFound(w, r)
		return
	}
	app := r.PostFormValue("app")
	back := func(n string) {
		target := phoneURL(id) + "?notice=" + n
		if name, ok := appNames[app]; ok {
			target += "&app=" + name
		}
		//nolint:gosec // the target is this phone's own page: a parsed number and fixed words
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
	if t.Status != bank.Pending {
		back("decided")
		return
	}
	ops := h.operators[sim.Operator]
	prompts, err := ops.Prompts(r.Context(), sim.Number)
	if err != nil {
		h.internal(w, err)
		return
	}
	if !slices.ContainsFunc(prompts, func(p bank.Prompt) bool { return p.ID == txID }) {
		http.NotFound(w, r)
		return
	}
	switch r.PostFormValue("action") {
	case "approve":
		h.decide(w, r, ops, txID, true, back)
	case "refuse":
		h.decide(w, r, ops, txID, false, back)
	case "ignore":
		h.ignore(t)
		back("ignored")
	case "show":
		h.unignore(txID)
		back("")
	default:
		http.Error(w, "action must be approve, refuse, ignore or show", http.StatusBadRequest)
	}
}

// simOf finds which of the phone's SIMs a transaction is on.
func simOf(sims []SIM, t bank.Transaction) (SIM, bool) {
	for _, sim := range sims {
		if sim.Operator == t.Operator && sim.Number == t.Phone {
			return sim, true
		}
	}
	return SIM{}, false
}

// decide finishes even if the browser goes away, so the money and the callback agree.
func (h *handler) decide(w http.ResponseWriter, r *http.Request, ops Operator, id string, approve bool, back func(string)) {
	err := ops.Answer(context.WithoutCancel(r.Context()), id, approve)
	switch {
	case errors.Is(err, bank.ErrDecided):
		back("decided")
	case errors.Is(err, bank.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		h.internal(w, err)
	case approve:
		back("approved")
	default:
		back("refused")
	}
}

// ignore hides a prompt, as a phone does once the USSD menu is closed; it still waits
// under Approvals until it expires.
func (s *Service) ignore(t bank.Transaction) {
	now := s.books.Clock().Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, until := range s.ignored {
		if !now.Before(until) {
			delete(s.ignored, id)
		}
	}
	s.ignored[t.ID] = t.ExpiresAt
}

func (s *Service) unignore(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ignored, id)
}

func (s *Service) isIgnored(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ignored[id]
	return ok
}

// events tells an open page to refresh when what it shows changes.
func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	_, sims, ok := h.device(w, r)
	if !ok {
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		h.logger.Warn("phone events", "err", err)
	}
	changes, stop := h.books.Watch()
	defer stop()
	last, err := h.fingerprint(r.Context(), sims)
	if err != nil {
		h.internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(": connected\n\n")); err != nil || rc.Flush() != nil {
		return
	}
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	for {
		var msg string
		select {
		case <-r.Context().Done():
			return
		case <-h.stopping:
			return
		case <-beat.C:
			msg = ": ping\n\n"
		case <-changes:
			now, err := h.fingerprint(r.Context(), sims)
			if err != nil {
				if r.Context().Err() == nil {
					h.logger.Error("phone events", "err", err)
				}
				return
			}
			if now == last {
				continue
			}
			last, msg = now, "event: change\ndata: \n\n"
		}
		if _, err := w.Write([]byte(msg)); err != nil || rc.Flush() != nil {
			return
		}
	}
}

// fingerprint changes whenever the phone's page would.
func (h *handler) fingerprint(ctx context.Context, sims []SIM) (string, error) {
	var b strings.Builder
	for _, sim := range sims {
		wallets, err := h.books.Wallets(ctx, sim.Operator, sim.Number)
		if err != nil {
			return "", err
		}
		for _, a := range wallets {
			b.WriteString(a.Currency + ":" + strconv.FormatInt(a.Balance, 10) + ";")
		}
		history, err := h.books.History(ctx, sim.Operator, sim.Number, historyLen)
		if err != nil {
			return "", err
		}
		for _, t := range history {
			b.WriteString(t.ID + ":" + string(t.Status) + ";")
		}
		prompts, err := h.operators[sim.Operator].Prompts(ctx, sim.Number)
		if err != nil {
			return "", err
		}
		for _, p := range prompts {
			b.WriteString(p.ID + ";")
		}
		inbox, err := h.store.inbox(ctx, sim.Operator, sim.Number, 1)
		if err != nil {
			return "", err
		}
		for _, m := range inbox {
			b.WriteString("sms:" + strconv.FormatInt(m.ID, 10) + ";")
		}
	}
	return b.String(), nil
}

func (s *Service) internal(w http.ResponseWriter, err error) {
	s.logger.Error("phone", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

type incoming struct {
	URL      string `json:"url"`
	Number   string `json:"number"`
	Operator string `json:"operator"`
	Payee    string `json:"payee"`
	Amount   string `json:"amount"`
}

// feed tells the phones list about every new payment waiting on any number, so a browser
// notification can lead to the right phone.
func (h *handler) feed(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		h.logger.Warn("phone feed", "err", err)
	}
	changes, stop := h.books.Watch()
	defer stop()
	seen, _, err := h.waiting(r.Context(), nil)
	if err != nil {
		h.internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(": connected\n\n")); err != nil || rc.Flush() != nil {
		return
	}
	beat := time.NewTicker(heartbeat)
	defer beat.Stop()
	for {
		var msg strings.Builder
		select {
		case <-r.Context().Done():
			return
		case <-h.stopping:
			return
		case <-beat.C:
			msg.WriteString(": ping\n\n")
		case <-changes:
			now, fresh, err := h.waiting(r.Context(), seen)
			if err != nil {
				if r.Context().Err() == nil {
					h.logger.Error("phone feed", "err", err)
				}
				return
			}
			seen = now
			for _, in := range fresh {
				data, err := json.Marshal(in)
				if err != nil {
					h.internal(w, err)
					return
				}
				msg.WriteString("event: prompt\ndata: " + string(data) + "\n\n")
			}
			if msg.Len() == 0 {
				continue
			}
		}
		if _, err := w.Write([]byte(msg.String())); err != nil || rc.Flush() != nil {
			return
		}
	}
}

// waiting lists the payments waiting on every number, and those not in seen.
func (s *Service) waiting(ctx context.Context, seen map[string]bool) (map[string]bool, []incoming, error) {
	now := map[string]bool{}
	var fresh []incoming
	for op, ops := range s.operators {
		prompts, err := ops.Waiting(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range prompts {
			now[p.ID] = true
			if seen != nil && !seen[p.ID] {
				fresh = append(fresh, incoming{URL: base + url.PathEscape(string(op)) + "/" + url.PathEscape(p.Phone),
					Number: p.Phone, Operator: operatorName(op), Payee: p.Payee, Amount: amount(p.Amount, p.Currency)})
			}
		}
	}
	return now, fresh, nil
}
