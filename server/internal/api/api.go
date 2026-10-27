// Package api exposes ingest and read endpoints over stdlib net/http.
package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/samiashi/llm-tracker/server/internal/db"
)

type Server struct {
	DB  *db.DB
	Log *slog.Logger
	// Version is this build's release tag: returned on every ingest so an
	// agent can tell it is behind, and to the dashboard as the upgrade target.
	Version string
	// Enroll names who is enrolling, which is how every machine gets its
	// ingest token.
	Enroll Verifier
}

// Routes builds the mux. WithSecurityHeaders is applied by main, around the
// gate, so its refusals carry the headers too.
func (s *Server) Routes(spa http.Handler) http.Handler {
	mux := http.NewServeMux()
	allowed := map[string][]string{}
	handle := func(method, path string, h http.HandlerFunc) {
		mux.HandleFunc(method+" "+path, h)
		// Any other method on a known path is a 405 naming the right one, not
		// a 404. The method-less pattern is less specific, so it answers only
		// when the real route does not.
		if allowed[path] == nil {
			mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
				allow := strings.Join(allowed[path], ", ")
				w.Header().Set("Allow", allow)
				writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("%s takes %s", path, allow))
			})
		}
		allowed[path] = append(allowed[path], method)
		if method == http.MethodGet {
			allowed[path] = append(allowed[path], http.MethodHead) // served by GET routes
		}
	}

	handle("POST", "/v1/ingest", s.handleIngest)
	handle("POST", "/v1/enroll", s.handleEnroll)
	handle("GET", "/v1/summary", s.windowed(s.handleSummary))
	handle("GET", "/v1/breakdown", s.windowed(s.handleBreakdown))
	handle("GET", "/v1/daily", s.windowed(s.handleDaily))
	handle("GET", "/v1/compare", s.windowed(s.handleCompare))
	handle("GET", "/v1/matrix", s.windowed(s.handleMatrix))
	handle("GET", "/v1/export.csv", s.windowed(s.handleExportCSV))
	handle("GET", "/v1/heatmap", s.windowed(s.handleHeatmap))
	handle("GET", "/v1/sessions/top", s.windowed(s.handleTopSessions))
	handle("GET", "/v1/daily/model", s.windowed(s.handleDailyByModel))
	handle("GET", "/v1/health/sources", s.handleSourceHealth)
	handle("GET", "/v1/agents", s.handleAgents)
	handle("GET", "/v1/unknown", s.handleUnknown)
	handle("GET", "/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// An unmatched /v1/ path is an API error: answered by the SPA, a typo'd
	// endpoint is HTML with a 200.
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no such endpoint: %s", r.URL.Path))
	})

	mux.Handle("/", spa)
	return s.withRecovery(s.withLogging(mux))
}

func window(r *http.Request) db.Window {
	q := r.URL.Query()
	start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
	end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
	var zone *time.Location
	if name := q.Get("timezone"); name != "" {
		zone, _ = time.LoadLocation(name)
	}
	return db.Window{From: q.Get("from"), To: q.Get("to"), Person: q.Get("person"), Start: start, End: end, Zone: zone}
}

// windowed refuses a range that is not two real days in order. The queries
// compare days as strings, so an unparsed to=2026-09-3 sorts after the 22nd,
// and 2026-04-31 fails only where a handler parses it, as a 500.
func (s *Server) windowed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if name := q.Get("timezone"); name != "" {
			if len(name) > 128 || name == "Local" {
				writeErr(w, http.StatusBadRequest, errors.New("timezone must be an IANA timezone name"))
				return
			}
			if _, err := time.LoadLocation(name); err != nil {
				writeErr(w, http.StatusBadRequest, errors.New("unknown timezone"))
				return
			}
		}
		win := window(r).Normalise()
		if q.Has("start") || q.Has("end") {
			start, se := strconv.ParseInt(q.Get("start"), 10, 64)
			end, ee := strconv.ParseInt(q.Get("end"), 10, 64)
			if se != nil || ee != nil || start <= 0 || end <= start || end-start != 86400 || end > time.Now().Unix()+60 {
				writeErr(w, http.StatusBadRequest, errors.New("start and end must be Unix seconds spanning exactly 24 hours, ending no later than now"))
				return
			}
			// Rollups cannot recover a partial day's usage. Refuse an incomplete
			// answer, including the comparison's preceding window, instead of $0.
			first, err := s.DB.DetailFrom(r.Context())
			if err != nil {
				s.writeInternal(w, "window", err)
				return
			}
			if r.URL.Path == "/v1/compare" {
				start -= 86400
			}
			if first != "" && time.Unix(start, 0).UTC().Format(time.DateOnly) < first {
				writeErr(w, http.StatusUnprocessableEntity, errors.New("this time range is unavailable because retention removed its per-event timestamps; choose a day-based range"))
				return
			}
		}
		from, ferr := time.Parse(time.DateOnly, win.From)
		to, terr := time.Parse(time.DateOnly, win.To)
		switch {
		case ferr != nil || terr != nil:
			writeErr(w, http.StatusBadRequest, errors.New("from and to must be real dates, as YYYY-MM-DD"))
		case q.Get("timezone") != "" && to.Sub(from).Hours()/24 >= 3650:
			writeErr(w, http.StatusBadRequest, errors.New("timezone-aware ranges are limited to 3650 days"))
		case from.After(to):
			writeErr(w, http.StatusBadRequest, fmt.Errorf("from %s is after to %s", win.From, win.To))
		default:
			h(w, r)
		}
	}
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	win := window(r)
	totals, err := s.DB.Totals(r.Context(), win)
	if err != nil {
		s.writeInternal(w, "summary", err)
		return
	}
	first, last, err := s.DB.DayRange(r.Context(), win.Person)
	if err != nil {
		s.writeInternal(w, "summary", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totals": totals,

		"history_first_day": first,
		"history_last_day":  last,
		// The release every agent is compared against, "" for a build that
		// is not one.
		"server_release": db.Release(s.Version),
	})
}

// handleBreakdown passes the dimension to db.Breakdown, whose allow-list keeps
// the query string out of the SQL.
func (s *Server) handleBreakdown(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "person"
	}
	groups, err := s.DB.Breakdown(r.Context(), window(r), by)
	if err != nil {
		if errors.Is(err, db.ErrUnknownDimension) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		s.writeInternal(w, "breakdown", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (s *Server) handleDaily(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Daily(r.Context(), window(r))
	if err != nil {
		s.writeInternal(w, "daily", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": rows})
}

func (s *Server) handleMatrix(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.DB.Matrix(r.Context(), window(r),
		q.Get("rows"), q.Get("cols"), atoi(q.Get("limit")))
	if err != nil {
		if errors.Is(err, db.ErrUnknownDimension) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		s.writeInternal(w, "matrix", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	c, err := s.DB.Compare(r.Context(), window(r))
	if err != nil {
		s.writeInternal(w, "compare", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	// Normalised here, so the filename names the range actually served: a
	// bare request still gets a 30-day window, and the filename is the only
	// record a saved file keeps of it.
	win := window(r).Normalise()
	rows, err := s.DB.Export(r.Context(), win)
	if err != nil {
		s.writeInternal(w, "exportCSV", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="llm-tracker-%s-to-%s.csv"`,
			safeDatePart(win.From), safeDatePart(win.To)))
	if win.End > 0 {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="llm-tracker-24h-%d-to-%d.csv"`, win.Start, win.End))
	}

	cw := csv.NewWriter(w)
	defer func() {
		cw.Flush()
		// A flush failure means a truncated download, which otherwise looks
		// like a complete file with missing rows.
		if err := cw.Error(); err != nil {
			s.Log.Error("csv export truncated", "err", err)
		}
	}()
	// New columns go last: a spreadsheet that reads by position must not
	// shift under them.
	_ = cw.Write([]string{"day", "person", "source", "model", "effort", "origin",
		"tokens", "input_tokens", "output_tokens", "cache_read_tokens",
		"cost_usd", "cost_basis", "responses", "unpriced_tokens"})
	for _, r := range rows {
		_ = cw.Write([]string{csvSafe(r.Day), csvSafe(r.Person), csvSafe(r.Source),
			csvSafe(r.Model), csvSafe(r.Effort), csvSafe(r.Origin),
			strconv.FormatInt(r.Tokens, 10), strconv.FormatInt(r.InputTokens, 10),
			strconv.FormatInt(r.OutputTokens, 10), strconv.FormatInt(r.CacheRead, 10),
			strconv.FormatFloat(r.CostUSD, 'f', 6, 64), csvSafe(r.CostBasis),
			strconv.FormatInt(r.Events, 10), strconv.FormatInt(r.UnpricedTokens, 10)})
	}
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Agents(r.Context())
	if err != nil {
		s.writeInternal(w, "agents", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": rows, "now": time.Now().Unix(),
		// The upgrade target: what the team should run, not what most of it
		// happens to.
		"server_version": s.Version,
	})
}

func (s *Server) handleHeatmap(w http.ResponseWriter, r *http.Request) {
	cells, offset, err := s.DB.Heatmap(r.Context(), window(r))
	if err != nil {
		s.writeInternal(w, "heatmap", err)
		return
	}
	from, err := s.DB.DetailFrom(r.Context())
	if err != nil {
		s.writeInternal(w, "heatmap", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Cells      []db.HeatCell `json:"cells"`
		DetailFrom string        `json:"detail_from,omitempty"`
		// The zone the hours are in, as minutes east of UTC: the server's own,
		// which the dashboard names beside the grid. A container left on UTC
		// shows its hours shifted by the team's whole offset, and this makes
		// that visible.
		UTCOffsetMinutes int    `json:"utc_offset_minutes"`
		TimeZone         string `json:"timezone,omitempty"`
	}{cells, from, offset, r.URL.Query().Get("timezone")})
}

func (s *Server) handleTopSessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.TopSessions(r.Context(), window(r), atoi(r.URL.Query().Get("limit")))
	if err != nil {
		s.writeInternal(w, "topSessions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": rows})
}

func (s *Server) handleDailyByModel(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.DailyByModel(r.Context(), window(r), atoi(r.URL.Query().Get("top")))
	if err != nil {
		s.writeInternal(w, "dailyByModel", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": rows})
}

// csvSafe neutralises spreadsheet formula injection. Every text cell comes
// from whatever a harness wrote, or from any enrolled machine, and
// Excel and Sheets execute a cell beginning =, +, - or @ as a formula the
// moment a colleague opens the file.
func csvSafe(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + v
	}
	return v
}

// safeDatePart admits a YYYY-MM-DD date and nothing else. The range reaches a
// response header, where a crafted link could otherwise choose the saved
// filename or break the header with a CRLF.
func safeDatePart(v string) string {
	if len(v) != len("2006-01-02") {
		return "range"
	}
	for i, r := range v {
		switch i {
		case 4, 7:
			if r != '-' {
				return "range"
			}
		default:
			if r < '0' || r > '9' {
				return "range"
			}
		}
	}
	return v
}

// atoi reads a list length, 0 when absent or unreadable: the db owns each
// list's default and cap.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (s *Server) handleSourceHealth(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.SourceHealth(r.Context())
	if err != nil {
		s.writeInternal(w, "sourceHealth", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": rows})
}

func (s *Server) handleUnknown(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.UnknownSources(r.Context())
	if err != nil {
		s.writeInternal(w, "unknown", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unknown": rows})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	// Every /v1 response is per-developer activity, so no intermediary may
	// store one and serve it to the next requester. Set here, not only as
	// WithSecurityHeaders' default, so no wrapper can relax it.
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr is for 4xx only: the message is sent verbatim, so nothing internal
// may be passed to it.
func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// writeInternal logs the real error and tells the client nothing: a driver
// error carries SQL, table names and file paths.
func (s *Server) writeInternal(w http.ResponseWriter, op string, err error) {
	s.Log.Error(op, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
