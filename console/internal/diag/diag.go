// Package diag says what the console may show of the unit's logs and what a support bundle may carry
// (docs/console.md, Diagnostics). patai-rootd reads the logs. This decides which lines belong to which
// source and takes credentials out, and both sides of the helper use it.
package diag

import (
	"bufio"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Source is a fixed set of the journal's lines. Nothing about it comes from a request but its id.
type Source struct {
	ID     string
	Units  []string // their own lines, and systemd's about them
	Idents []string // syslog identifiers
	Kernel bool
	Owner  bool // only an owner may read it
	Bundle bool // a support bundle carries it
	Scan   bool // no match says which lines are its, so they're picked out of everything
}

// Output is what the running workload printed, which Docker keeps, not the journal.
const Output = "output"

// SignIns is the one source with who logged in and what they ran with sudo.
const SignIns = "sign-ins"

var Sources = []Source{
	// Picked out of everything too: on a unit someone uses over SSH, sign-ins can be most of the newest lines.
	{ID: "all", Bundle: true, Scan: true},
	{ID: "kernel", Kernel: true, Bundle: true},
	{ID: "console", Bundle: true, Units: []string{"patai-api.service", "patai-rootd.service", "patai-report.service",
		"patai-screen.service", "patai-identity.service", "patai-factory-reset.service"}},
	{ID: "updates", Bundle: true, Units: []string{"patai-update.service", "patai-update-install.service", "rauc.service",
		"patai-rollback-report.service", "nv-l4t-bootloader-config.service", "l4t-rootfs-validation-config.service"}},
	{ID: "workloads", Bundle: true, Units: []string{"patai-workload.service", "patai-fetch.service",
		"patai-data-disk.service", "patai-data-gate.service", "data.mount", "docker.service", "docker.socket",
		"containerd.service"}},
	{ID: "network", Bundle: true, Units: []string{"systemd-networkd.service", "patai-network.service",
		"patai-firewall.service", "systemd-resolved.service", "systemd-timesyncd.service", "nvrtc-sync-boot.service"}},
	{ID: "power", Bundle: true, Units: []string{"patai-power-mode.service", "nvpmodel.service", "nvfancontrol.service",
		"patai-thermal-log.service"}},
	{ID: SignIns, Owner: true, Scan: true},
}

// Sign-ins are sshd's, sudo's and logind's lines, and systemd's about login sessions and the user managers they
// start. sshd prints what was typed at a login, which is sometimes a password typed one prompt early, and sudo
// the commands it ran.
var (
	signInPrograms = []string{"sshd", "sudo", "su", "login", "systemd-logind"}
	signInUnits    = []string{"ssh.service", "systemd-logind.service"}
	sessionUnit    = regexp.MustCompile(`^(session-[^/]+\.scope|user@\d+\.service|user-\d+\.slice|user-runtime-dir@\d+\.service)$`)
)

// SignIn says whether a line is a sign-in's, by its fields.
func SignIn(raw map[string]string) bool {
	if slices.Contains(signInUnits, raw["_SYSTEMD_UNIT"]) {
		return true
	}
	for _, field := range []string{"SYSLOG_IDENTIFIER", "_COMM"} {
		if slices.Contains(signInPrograms, raw[field]) {
			return true
		}
	}
	return sessionUnit.MatchString(raw["_SYSTEMD_UNIT"]) || sessionUnit.MatchString(raw["UNIT"])
}

// SignInText says whether a line of journalctl's short format is a sign-in's, for lines kept as text.
var SignInText = regexp.MustCompile(`^\w{3} [ \d]\d \d\d:\d\d:\d\d \S+ (?:(?:sshd|sudo|su|login|systemd-logind)[\[:]|.*\b(?:session-\d+\.scope|[Ss]ession \d+ of user|user@\d+\.service|User Manager for UID|[Uu]ser [Ss]lice of UID|user-runtime-dir@))`)

func Find(id string) (Source, bool) {
	i := slices.IndexFunc(Sources, func(s Source) bool { return s.ID == id })
	if i < 0 {
		return Source{}, false
	}
	return Sources[i], true
}

// Matches are journalctl's arguments for the source: the same field twice is either, and + separates the rest.
func (s Source) Matches() []string {
	if s.Scan {
		return nil
	}
	if s.Kernel {
		return []string{"_TRANSPORT=kernel"}
	}
	var out []string
	group := func(field string, values []string) {
		if len(values) == 0 {
			return
		}
		if len(out) > 0 {
			out = append(out, "+")
		}
		for _, v := range values {
			out = append(out, field+"="+v)
		}
	}
	group("_SYSTEMD_UNIT", s.Units)
	group("UNIT", s.Units)
	group("SYSLOG_IDENTIFIER", s.Idents)
	return out
}

// Query is what the console asks the helper for. Its checks are the helper's to make.
type Query struct {
	Source    string `json:"source"`
	Boot      string `json:"boot,omitempty"`  // this boot if empty
	Count     int    `json:"count"`           // at most this many lines
	After     string `json:"after,omitempty"` // only what came after this cursor, oldest first; else the newest
	Important bool   `json:"important,omitempty"`
	Stack     string `json:"stack,omitempty"` // for Output
}

// Limits on one read, whoever asks. The page asks for far less.
const (
	MaxCount   = 20000
	MaxRead    = 4 << 20
	MaxMessage = 4 << 10
	ReadTime   = 10 * time.Second
)

var (
	cursorShape = regexp.MustCompile(`^s=[0-9a-f]{32};i=[0-9a-f]{1,16};b=[0-9a-f]{32};m=[0-9a-f]{1,16};t=[0-9a-f]{1,16};x=[0-9a-f]{1,16}$`)
	bootShape   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func ValidCursor(s string) bool { return cursorShape.MatchString(s) }
func ValidBoot(s string) bool   { return bootShape.MatchString(s) }

// Check says whether the query is one the helper may run.
func (q Query) Check() bool {
	if q.Count < 1 || q.Count > MaxCount {
		return false
	}
	if q.Source == Output {
		return q.Boot == "" && (q.After == "" || ValidStamp(q.After))
	}
	_, ok := Find(q.Source)
	return ok && (q.Boot == "" || ValidBoot(q.Boot)) && (q.After == "" || ValidCursor(q.After))
}

// Entry is one line, as the page and a bundle get it.
type Entry struct {
	Cursor   string `json:"cursor,omitempty"`
	Time     int64  `json:"time_us"` // by the unit's clock, which may have been wrong
	Priority int    `json:"priority"`
	Unit     string `json:"unit,omitempty"`
	Ident    string `json:"ident,omitempty"`
	PID      string `json:"pid,omitempty"`
	Stream   string `json:"stream,omitempty"` // for Output: stdout or stderr
	Message  string `json:"message"`
}

// Page is one answer. Cursor is where following carries on from: past every line read, shown or not.
type Page struct {
	Entries []Entry `json:"entries"`
	Cursor  string  `json:"cursor"`
	More    bool    `json:"more"`             // a limit stopped it with more to read
	Absent  string  `json:"absent,omitempty"` // why there's nothing to read, if there isn't
	Gap     bool    `json:"gap,omitempty"`    // following couldn't reach back to its cursor, so lines were missed
	First   string  `json:"-"`                // the first line read, which is the newest when reading backwards
}

// Boot is one of this slot's boots, with its first and last line's time.
type Boot struct {
	ID    string `json:"id"`
	Index int    `json:"index"` // 0 is this one, -1 the one before
	First string `json:"first,omitempty"`
	Last  string `json:"last,omitempty"`
}

// ReadBoots reads `journalctl --list-boots`, run with TZ=UTC and LANG=C.
func ReadBoots(out string) []Boot {
	boots := []Boot{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !ValidBoot(fields[1]) {
			continue
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		boot := Boot{ID: fields[1], Index: index}
		// "Mon 2026-11-02 23:00:31 UTC—Tue 2026-11-03 00:42:10 UTC"
		if len(fields) >= 8 {
			boot.First = stamp(fields[3] + " " + fields[4])
			boot.Last = stamp(fields[6] + " " + fields[7])
		}
		boots = append(boots, boot)
	}
	return boots
}

func stamp(s string) string {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ReadJournal reads journalctl's JSON, in the order it comes, keeping up to count of the source's lines, and reading
// at most MaxRead bytes. Lines that aren't the source's, sign-ins above all, are read and left out. A source picked
// out of everything reads up to MaxCount lines to find its own; any other reads count, since journalctl matched them.
func ReadJournal(r io.Reader, source Source, count int, codes []string) Page {
	page := Page{Entries: []Entry{}}
	scan := count
	if source.Scan {
		scan = MaxCount
	}
	reader := bufio.NewReaderSize(io.LimitReader(r, MaxRead+1), 64<<10)
	var read int64
	for seen := 0; len(page.Entries) < count && seen < scan; {
		line, err := reader.ReadBytes('\n')
		read += int64(len(line))
		if read > MaxRead {
			page.More = true
			return page
		}
		if len(line) > 0 {
			if entry, raw, ok := parseEntry(line); ok {
				seen++
				page.Cursor = entry.Cursor
				if page.First == "" {
					page.First = entry.Cursor
				}
				if source.Keeps(raw) {
					entry.Message = Redact(entry.Message, codes)
					page.Entries = append(page.Entries, entry)
				}
			}
		}
		if err != nil {
			return page
		}
	}
	// Stopped at a limit: the next line, if there is one, says there's more.
	_, err := reader.Peek(1)
	page.More = err == nil
	return page
}

// Keeps says whether a line belongs in the source. journalctl chose by its matches already: this is for what
// matches can't say, that sign-ins are in no other source.
func (s Source) Keeps(raw map[string]string) bool {
	if s.ID == SignIns {
		return SignIn(raw)
	}
	return !SignIn(raw)
}

func parseEntry(line []byte) (Entry, map[string]string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil {
		return Entry{}, nil, false
	}
	raw := map[string]string{}
	for _, name := range []string{"__CURSOR", "__REALTIME_TIMESTAMP", "PRIORITY", "SYSLOG_IDENTIFIER", "_COMM",
		"_SYSTEMD_UNIT", "UNIT", "_PID", "MESSAGE"} {
		if value, ok := fields[name]; ok {
			raw[name] = fieldText(value)
		}
	}
	if !ValidCursor(raw["__CURSOR"]) {
		return Entry{}, nil, false
	}
	at, _ := strconv.ParseInt(raw["__REALTIME_TIMESTAMP"], 10, 64)
	priority, err := strconv.Atoi(raw["PRIORITY"])
	if err != nil {
		priority = 6
	}
	ident := raw["SYSLOG_IDENTIFIER"]
	if ident == "" {
		ident = raw["_COMM"]
	}
	unit := raw["_SYSTEMD_UNIT"]
	if unit == "" {
		unit = raw["UNIT"]
	}
	return Entry{Cursor: raw["__CURSOR"], Time: at, Priority: priority, Unit: unit, Ident: ident, PID: raw["_PID"],
		Message: cut(raw["MESSAGE"])}, raw, true
}

// fieldText is a field as text. journalctl gives a string, an array of bytes for one that isn't text, an array
// of either for a field given twice, and null for one too big to print.
func fieldText(value json.RawMessage) string {
	var s string
	if json.Unmarshal(value, &s) == nil {
		return s
	}
	var bytes []byte
	var numbers []int
	if json.Unmarshal(value, &numbers) == nil {
		for _, n := range numbers {
			bytes = append(bytes, byte(n))
		}
		return strings.ToValidUTF8(string(bytes), "�")
	}
	var many []json.RawMessage
	if json.Unmarshal(value, &many) == nil && len(many) > 0 {
		return fieldText(many[0])
	}
	return ""
}

func cut(s string) string {
	if len(s) <= MaxMessage {
		return s
	}
	s = s[:MaxMessage]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + " [cut]"
}

// Text is a line as a person reads it in a file, in UTC.
func (e Entry) Text() string {
	at := time.UnixMicro(e.Time).UTC().Format("2006-01-02T15:04:05.000000Z")
	who := e.Ident
	if who == "" {
		who = e.Stream
	}
	if e.PID != "" {
		who += "[" + e.PID + "]"
	}
	level := ""
	if e.Priority <= 3 {
		level = " error:"
	} else if e.Priority == 4 {
		level = " warning:"
	}
	return at + " " + who + ":" + level + " " + strings.ReplaceAll(e.Message, "\n", "\n    ")
}
