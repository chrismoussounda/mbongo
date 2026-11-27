package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/chrismoussounda/mbongo/internal/faults"
)

// Delays used when a rule gives none.
const (
	defaultSlow = 30 * time.Second
	defaultLate = time.Minute
)

func (h *handler) addRule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Operator     string  `json:"operator"`
		Operation    string  `json:"operation"`
		Phone        string  `json:"phone"`
		Fault        string  `json:"fault"`
		Times        *int    `json:"times"`
		DelaySeconds float64 `json:"delaySeconds"`
	}
	if !read(w, r, &in) {
		return
	}
	switch {
	case in.Operator != "mtn" && in.Operator != "airtel":
		h.refuse(w, "operator must be mtn or airtel")
		return
	case in.Fault == faults.Ambiguous && in.Operator != "airtel":
		h.refuse(w, "ambiguous is Airtel's TA status; MTN has none")
		return
	case in.Times != nil && *in.Times < 0:
		h.refuse(w, "times must be 0 (until cleared) or more")
		return
	case in.DelaySeconds < 0:
		h.refuse(w, "delaySeconds can't be negative")
		return
	}
	rule := faults.Rule{Operator: in.Operator, Operation: in.Operation, Phone: in.Phone, Fault: in.Fault, Times: 1,
		Delay: time.Duration(in.DelaySeconds * float64(time.Second))}
	if in.Times != nil {
		rule.Times = *in.Times
	}
	if rule.Delay == 0 {
		switch rule.Fault {
		case faults.Slow:
			rule.Delay = defaultSlow
		case faults.CallbackLate:
			rule.Delay = defaultLate
		}
	}
	rule, err := h.faults.Add(r.Context(), rule)
	if errors.Is(err, faults.ErrUnknownFault) {
		h.refuse(w, "unknown fault; see docs/faults.md")
		return
	}
	if errors.Is(err, faults.ErrOperation) {
		h.refuse(w, "callback faults take the operation callback, other faults another one")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	reply(w, http.StatusCreated, rule)
}

func (h *handler) listRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.faults.List(r.Context())
	if err != nil {
		h.internal(w, err)
		return
	}
	reply(w, http.StatusOK, rules)
}

func (h *handler) removeRule(w http.ResponseWriter, r *http.Request) {
	var id int64
	if raw := r.PathValue("id"); raw != "" {
		var err error
		if id, err = strconv.ParseInt(raw, 10, 64); err != nil || id <= 0 {
			h.refuse(w, "id must be a rule's id")
			return
		}
	}
	err := h.faults.Remove(r.Context(), id)
	if errors.Is(err, faults.ErrNotFound) {
		reply(w, http.StatusNotFound, map[string]string{"error": "no such rule"})
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
