package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"patai/console/internal/diag"
	"patai/console/internal/rootd"
)

// The Logs page (docs/console.md, Diagnostics): this slot's journal by source, and what the workload printed.
// An operator's, and sign-ins an owner's. The helper reads them; nothing here can name a unit, a file or a match.

const (
	pageLines     = 2000 // the most the page asks for at once
	followLines   = 500  // the most one look sends while following, before the next
	followPoll    = time.Second
	followBeat    = 15 * time.Second
	followFor     = 30 * time.Minute
	followsOnUnit = 4
	followsEach   = 2
)

// follows counts streams, so a forgotten tab or a script can't keep the helper reading for everyone.
type follows struct {
	mu    sync.Mutex
	total int
	by    map[string]int
}

func (f *follows) take(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.by == nil {
		f.by = map[string]int{}
	}
	if f.total >= followsOnUnit || f.by[key] >= followsEach {
		return false
	}
	f.total++
	f.by[key]++
	return true
}

func (f *follows) give(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.total--
	if f.by[key]--; f.by[key] <= 0 {
		delete(f.by, key)
	}
}

// followStall is how long one write to a stream may take. A variable for tests.
var followStall = 10 * time.Second

type readResult struct {
	page diag.Page
	err  error
}

type logSource struct {
	ID     string `json:"id"`
	Bundle bool   `json:"bundle"`
}

// logIndex is what can be read: the sources this person may, and this slot's boots.
func (s *Server) logIndex(w http.ResponseWriter, r *http.Request, p *Principal) {
	sources := []logSource{}
	for _, source := range diag.Sources {
		if !source.Owner || p.has("owner") {
			sources = append(sources, logSource{ID: source.ID, Bundle: source.Bundle})
		}
	}
	sources = append(sources, logSource{ID: diag.Output})
	output, ok := s.ask(w, r, rootd.Request{Verb: "log-boots"})
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources, "boots": json.RawMessage(output)})
}

// logQuery reads what to show from the request, and says no to a source this person may not read.
func (s *Server) logQuery(w http.ResponseWriter, r *http.Request, p *Principal) (diag.Query, string, bool) {
	values := r.URL.Query()
	q := diag.Query{Source: r.PathValue("source"), Boot: values.Get("boot"), Important: values.Get("important") == "1"}
	role := "operator"
	if q.Source == diag.Output {
		q.Stack = values.Get("stack")
	} else if source, ok := diag.Find(q.Source); !ok {
		fail(w, http.StatusNotFound, "not-found", "there's no such log")
		return q, "", false
	} else if source.Owner {
		role = "owner"
	}
	if !p.has(role) {
		fail(w, http.StatusForbidden, "forbidden", "your role can't read that log")
		return q, "", false
	}
	return q, role, true
}

func (s *Server) readLogs(ctx context.Context, q diag.Query) (diag.Page, error) {
	verb := "logs"
	if q.Source == diag.Output {
		verb = "workload-logs"
	}
	ctx, cancel := context.WithTimeout(ctx, rootdWait)
	defer cancel()
	resp, err := s.Rootd.Call(ctx, rootd.Request{Verb: verb, Logs: &q})
	if err != nil {
		return diag.Page{}, err
	}
	var page diag.Page
	if err := json.Unmarshal([]byte(resp.Output), &page); err != nil {
		return diag.Page{}, err
	}
	if page.Entries == nil {
		page.Entries = []diag.Entry{}
	}
	return page, nil
}

// getLogs is the newest lines of a source, oldest first, and the cursor to follow it from.
func (s *Server) getLogs(w http.ResponseWriter, r *http.Request, p *Principal) {
	q, _, ok := s.logQuery(w, r, p)
	if !ok {
		return
	}
	q.Count = 200
	if n, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil {
		q.Count = min(max(n, 1), pageLines)
	}
	if !q.Check() {
		fail(w, http.StatusBadRequest, "bad-request", "that isn't a log this unit can read")
		return
	}
	page, err := s.readLogs(r.Context(), q)
	if err != nil {
		fail(w, http.StatusBadGateway, "rootd", "the unit couldn't read that log: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// followLogs sends a source's new lines as server-sent events, each with its cursor as its id, so a
// reconnect with Last-Event-ID carries on where it stopped. It ends after followFor, or when the login does.
func (s *Server) followLogs(w http.ResponseWriter, r *http.Request, p *Principal) {
	q, role, ok := s.logQuery(w, r, p)
	if !ok {
		return
	}
	q.After = r.Header.Get("Last-Event-ID")
	if q.After == "" {
		q.After = r.URL.Query().Get("after")
	}
	q.Count = followLines
	if q.Boot != "" || q.After == "" || !q.Check() {
		fail(w, http.StatusBadRequest, "bad-request", "following starts from a line of this boot's")
		return
	}
	key := p.Session
	if key == "" {
		key = p.Name
	}
	if !s.follows.take(key) {
		fail(w, http.StatusTooManyRequests, "busy", "too many logs are being followed on this unit now")
		return
	}
	defer s.follows.give(key)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// The deadline covers one write and is taken off after it: over HTTP/2 one that runs out resets the stream even
	// while nothing is being written, which a quiet log would otherwise do 10 s after each line.
	send := func(text string) bool {
		rc.SetWriteDeadline(time.Now().Add(followStall))
		_, err := io.WriteString(w, text)
		ok := err == nil && rc.Flush() == nil
		rc.SetWriteDeadline(time.Time{})
		return ok
	}
	end := func(reason string) { send("event: end\ndata: " + strconv.Quote(reason) + "\n\n") }
	if !send("retry: 3000\n\n") {
		return
	}

	allowed := func() bool {
		again := s.principal(r)
		return again != nil && again.has(role)
	}
	until, beat := time.Now().Add(followFor), time.Now().Add(followBeat)
	var wait time.Duration
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(wait):
		}
		wait = followPoll
		// Looked at again each time, without counting as activity: a logout or a lost role ends it.
		if !allowed() {
			end("login")
			return
		}
		if time.Now().After(until) {
			end("time")
			return
		}
		// A read can take seconds. The login is still looked at each second meanwhile, and again before sending.
		done := make(chan readResult, 1)
		go func(q diag.Query) {
			page, err := s.readLogs(r.Context(), q)
			done <- readResult{page, err}
		}(q)
		var got readResult
	reading:
		for {
			select {
			case got = <-done:
				break reading
			case <-r.Context().Done():
				return
			case <-time.After(followPoll):
				if !allowed() {
					end("login")
					return
				}
			}
		}
		if !allowed() {
			end("login")
			return
		}
		page, err := got.page, got.err
		if err != nil {
			if r.Context().Err() != nil || !send("event: problem\ndata: "+strconv.Quote(err.Error())+"\n\n") {
				return
			}
			wait = 5 * followPoll
			continue
		}
		var out strings.Builder
		if page.Gap {
			out.WriteString("event: gap\ndata: \"missed\"\n\n")
		}
		from, last := q.After, q.After
		for _, entry := range page.Entries {
			data, _ := json.Marshal(entry)
			out.WriteString("id: " + entry.Cursor + "\nevent: entry\ndata: " + string(data) + "\n\n")
			last = entry.Cursor
		}
		if page.Cursor != "" && page.Cursor != last {
			// Past lines that aren't this source's: an id alone moves a reconnect on without an event.
			out.WriteString("id: " + page.Cursor + "\n\n")
			last = page.Cursor
		}
		q.After = last
		if out.Len() > 0 || time.Now().After(beat) {
			if out.Len() == 0 {
				out.WriteString(": still here\n\n")
			}
			if !send(out.String()) {
				return
			}
			beat = time.Now().Add(followBeat)
		}
		if page.More && last != from {
			// Catching up: the next look comes at once rather than leaving lines behind. One that got nowhere
			// waits as usual, so a stuck read can't spin.
			wait = 100 * time.Millisecond
		}
	}
}
