package report

import "context"

// Limits are what a report cannot tell, because Mbongo only sees the client from the outside.
var Limits = []string{
	"Mbongo sees the calls a client makes and the callbacks it sends, nothing else. It cannot see the client's own books: whether an order was delivered, marked paid, or paid out twice from them is for the client's own tests.",
	"A callback trusted without a status call means no status call followed it; Mbongo cannot tell whether the client acted on it.",
	"Payments still pending are not judged yet; run the report again once they have ended.",
}

// Summary is a run's report for every client, or for the one asked for.
type Summary struct {
	Run      Run            `json:"run"`
	Errors   int            `json:"errors"`
	Warnings int            `json:"warnings"`
	Clients  []ClientReport `json:"clients"`
	Limits   []string       `json:"limits"`
}

// Summarize reports on a run, the latest when run is 0; operator and client, when set, keep
// only that client.
func (rec *Recorder) Summarize(ctx context.Context, run int64, ledgers map[string]Ledger, operator, client string) (Summary, error) {
	if run == 0 {
		latest, err := rec.Latest(ctx)
		if err != nil {
			return Summary{}, err
		}
		run = latest
	}
	runs, err := rec.Runs(ctx)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Clients: []ClientReport{}, Limits: Limits}
	for _, r := range runs {
		if r.ID == run {
			s.Run = r
		}
	}
	reports, err := rec.Build(ctx, run, ledgers)
	if err != nil {
		return s, err
	}
	for _, r := range reports {
		if (operator != "" && r.Operator != operator) || (client != "" && r.Client != client) {
			continue
		}
		s.Clients = append(s.Clients, r)
		s.Errors += r.Errors
		s.Warnings += r.Warnings
	}
	return s, nil
}
