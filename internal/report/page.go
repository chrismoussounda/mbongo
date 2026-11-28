package report

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/*
var static embed.FS

const base = "/_mbongo/report/"

// congo is Brazzaville's time, UTC+1 all year.
var congo = time.FixedZone("WAT", 60*60)

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"clock": func(t time.Time) string { return t.In(congo).Format("15:04:05") },
	"day":   func(t time.Time) string { return t.In(congo).Format("2 Jan 2006 15:04") },
}).ParseFS(templateFiles, "templates/*.html"))

// groupTitles name the groups in the order the page shows them.
var groupTitles = []struct{ Group, Title string }{
	{DoubleMoney, "Money moved twice"},
	{Trust, "Trust and follow-up"},
	{Refunds, "Refunds"},
	{Hygiene, "API hygiene"},
}

var operatorNames = map[string]string{"mtn": "MTN MoMo", "airtel": "Airtel Money"}

type runLink struct {
	ID      int64
	Started time.Time
	URL     string
	Current bool
}

type clientRow struct {
	ClientReport
	OperatorName string
	URL          string
}

type indexView struct {
	Run      Run
	Runs     []runLink
	Errors   int
	Warnings int
	Clients  []clientRow
	Limits   []string
}

type shownFinding struct {
	Finding
	Calls []Call
}

type group struct {
	Title    string
	Findings []shownFinding
}

type clientView struct {
	Run          Run
	Runs         []runLink
	Report       ClientReport
	OperatorName string
	Groups       []group
	Calls        []Call
	Callbacks    []Callback
	Limits       []string
	Back         string
}

type missingView struct{ What string }

type page struct {
	rec     *Recorder
	ledgers map[string]Ledger
	logger  *slog.Logger
}

// Routes serves the report pages, open to anyone who can reach Mbongo like the phone pages.
func (rec *Recorder) Routes(mux *http.ServeMux, ledgers map[string]Ledger, logger *slog.Logger) {
	p := &page{rec: rec, ledgers: ledgers, logger: logger}
	mux.HandleFunc("GET "+base+"static/report.css", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, static, "static/report.css")
	})
	mux.HandleFunc("GET "+base+"{$}", p.index)
	mux.HandleFunc("GET "+base+"{operator}/{client}", p.client)
}

// run is the run asked for in the query, or the latest; ok is false once the answer is sent.
func (p *page) run(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("run")
	if raw == "" {
		run, err := p.rec.Latest(r.Context())
		if err != nil {
			p.fail(w, err)
			return 0, false
		}
		return run, true
	}
	run, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || run <= 0 {
		p.render(w, http.StatusNotFound, "missing", missingView{What: "No such run."})
		return 0, false
	}
	return run, true
}

func (p *page) runLinks(ctx context.Context, current int64, path string) ([]runLink, error) {
	runs, err := p.rec.Runs(ctx)
	if err != nil {
		return nil, err
	}
	links := make([]runLink, len(runs))
	for i, r := range runs {
		links[i] = runLink{ID: r.ID, Started: r.Started, URL: path + "?run=" + strconv.FormatInt(r.ID, 10),
			Current: r.ID == current}
	}
	return links, nil
}

func (p *page) index(w http.ResponseWriter, r *http.Request) {
	run, ok := p.run(w, r)
	if !ok {
		return
	}
	s, err := p.rec.Summarize(r.Context(), run, p.ledgers, "", "")
	if errors.Is(err, ErrNoRun) {
		p.render(w, http.StatusNotFound, "missing", missingView{What: "No such run."})
		return
	}
	if err != nil {
		p.fail(w, err)
		return
	}
	v := indexView{Run: s.Run, Errors: s.Errors, Warnings: s.Warnings, Limits: s.Limits}
	if v.Runs, err = p.runLinks(r.Context(), s.Run.ID, base); err != nil {
		p.fail(w, err)
		return
	}
	for _, c := range s.Clients {
		v.Clients = append(v.Clients, clientRow{ClientReport: c, OperatorName: operatorNames[c.Operator],
			URL: base + url.PathEscape(c.Operator) + "/" + url.PathEscape(c.Client) + "?run=" + strconv.FormatInt(s.Run.ID, 10)})
	}
	p.render(w, http.StatusOK, "index", v)
}

func (p *page) client(w http.ResponseWriter, r *http.Request) {
	run, ok := p.run(w, r)
	if !ok {
		return
	}
	operator, client := r.PathValue("operator"), r.PathValue("client")
	s, err := p.rec.Summarize(r.Context(), run, p.ledgers, operator, client)
	if errors.Is(err, ErrNoRun) {
		p.render(w, http.StatusNotFound, "missing", missingView{What: "No such run."})
		return
	}
	if err != nil {
		p.fail(w, err)
		return
	}
	if len(s.Clients) == 0 {
		p.render(w, http.StatusNotFound, "missing", missingView{What: "This client made no call in run " +
			strconv.FormatInt(s.Run.ID, 10) + "."})
		return
	}
	v := clientView{Run: s.Run, Report: s.Clients[0], OperatorName: operatorNames[operator], Limits: s.Limits,
		Back: base + "?run=" + strconv.FormatInt(s.Run.ID, 10)}
	if v.Runs, err = p.runLinks(r.Context(), s.Run.ID, r.URL.Path); err != nil {
		p.fail(w, err)
		return
	}
	calls, err := p.rec.Calls(r.Context(), s.Run.ID)
	if err != nil {
		p.fail(w, err)
		return
	}
	byID := map[int64]Call{}
	for _, c := range calls {
		if c.Operator == operator && c.Client == client {
			v.Calls = append(v.Calls, c)
			byID[c.ID] = c
		}
	}
	cbs, err := p.rec.Callbacks(r.Context(), s.Run.ID)
	if err != nil {
		p.fail(w, err)
		return
	}
	for _, cb := range cbs {
		if cb.Operator == operator && cb.Client == client {
			v.Callbacks = append(v.Callbacks, cb)
		}
	}
	for _, g := range groupTitles {
		shown := group{Title: g.Title}
		for _, f := range v.Report.Findings {
			if f.Group != g.Group {
				continue
			}
			sf := shownFinding{Finding: f}
			for _, id := range f.Calls {
				sf.Calls = append(sf.Calls, byID[id])
			}
			shown.Findings = append(shown.Findings, sf)
		}
		v.Groups = append(v.Groups, shown)
	}
	p.render(w, http.StatusOK, "client", v)
}

func (p *page) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		p.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (p *page) fail(w http.ResponseWriter, err error) {
	p.logger.Error("report page", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
