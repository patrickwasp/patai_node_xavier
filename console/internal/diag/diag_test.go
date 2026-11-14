package diag

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const cursor = "s=a7838606f9e64af1abe0af30db30697b;i=96e;b=42a447525ef648fc80bf40d169b2b6de;m=18c0d9615;t=65f54a848f4f0;x=f710eda4f8c4bf67"

func line(i int, fields string) string {
	c := strings.Replace(cursor, "i=96e", fmt.Sprintf("i=%x", i), 1)
	return fmt.Sprintf(`{"__CURSOR":%q,"__REALTIME_TIMESTAMP":"1793667065509104","PRIORITY":"6",%s}`+"\n", c, fields)
}

func TestMatches(t *testing.T) {
	console, _ := Find("console")
	got := strings.Join(console.Matches(), " ")
	if !strings.HasPrefix(got, "_SYSTEMD_UNIT=patai-api.service _SYSTEMD_UNIT=patai-rootd.service") ||
		!strings.Contains(got, " + UNIT=patai-api.service UNIT=patai-rootd.service") {
		t.Fatalf("console matches: %s", got)
	}
	// Sign-ins are picked out of everything: no match can say which of systemd's lines are about a login.
	signIns, _ := Find(SignIns)
	if got := signIns.Matches(); len(got) != 0 || !signIns.Scan {
		t.Fatalf("sign-in matches: %v", got)
	}
	kernel, _ := Find("kernel")
	if got := kernel.Matches(); !slices.Equal(got, []string{"_TRANSPORT=kernel"}) {
		t.Fatalf("kernel matches: %v", got)
	}
	all, _ := Find("all")
	if len(all.Matches()) != 0 || !all.Scan {
		t.Fatal("everything has no matches, and reads on past sign-ins")
	}
	for _, s := range Sources {
		if s.ID == SignIns && (s.Bundle || !s.Owner) || s.ID != SignIns && s.Owner {
			t.Fatalf("%s: only sign-ins are an owner's, and they never go in a bundle", s.ID)
		}
	}
}

func TestQueryChecks(t *testing.T) {
	for _, q := range []Query{
		{Source: "all", Count: 200},
		{Source: "kernel", Count: MaxCount, Boot: "42a447525ef648fc80bf40d169b2b6de"},
		{Source: "console", Count: 1, After: cursor},
		{Source: Output, Count: 500, Stack: "llama-cpp", After: "2026-11-03T00:42:10.123456789Z"},
		{Source: Output, Count: 500, Stack: "llama-cpp", After: "2026-11-03T00:42:10.123456789Z#3"},
	} {
		if !q.Check() {
			t.Errorf("refused %+v", q)
		}
	}
	for _, q := range []Query{
		{Source: "all", Count: 0},
		{Source: "all", Count: MaxCount + 1},
		{Source: "everything", Count: 10},
		{Source: "all", Count: 10, Boot: "-1"},
		{Source: "all", Count: 10, Boot: "--file=/etc/shadow"},
		{Source: "all", Count: 10, After: "s=1;--merge"},
		{Source: "all", Count: 10, After: cursor + "\n--directory=/"},
		{Source: Output, Count: 10, After: "yesterday"},
		{Source: Output, Count: 10, Boot: "42a447525ef648fc80bf40d169b2b6de"},
	} {
		if q.Check() {
			t.Errorf("took %+v", q)
		}
	}
}

func TestSignInsAreTheirOwn(t *testing.T) {
	input := line(1, `"SYSLOG_IDENTIFIER":"sshd","_SYSTEMD_UNIT":"session-4.scope","MESSAGE":"Invalid user hunter2 from 192.168.4.177"`) +
		line(2, `"SYSLOG_IDENTIFIER":"sudo","MESSAGE":"admin : COMMAND=/usr/bin/thing --secret"`) +
		line(3, `"SYSLOG_IDENTIFIER":"systemd","_SYSTEMD_UNIT":"init.scope","UNIT":"patai-api.service","MESSAGE":"Started Console and API."`) +
		line(4, `"_COMM":"sshd","_SYSTEMD_UNIT":"ssh.service","MESSAGE":"Accepted publickey for admin"`) +
		line(5, `"SYSLOG_IDENTIFIER":"patai-update","_SYSTEMD_UNIT":"patai-update.service","MESSAGE":"up to date"`) +
		line(6, `"SYSLOG_IDENTIFIER":"systemd","_SYSTEMD_UNIT":"init.scope","UNIT":"session-4.scope","MESSAGE":"Started Session 4 of user admin."`) +
		line(7, `"SYSLOG_IDENTIFIER":"systemd","_SYSTEMD_UNIT":"user@1000.service","MESSAGE":"Reached target Default."`) +
		line(8, `"SYSLOG_IDENTIFIER":"systemd","_SYSTEMD_UNIT":"init.scope","UNIT":"user-1000.slice","MESSAGE":"Created slice User Slice of UID 1000."`)
	all, _ := Find("all")
	page := ReadJournal(strings.NewReader(input), all, 100, nil)
	if len(page.Entries) != 2 || page.Entries[0].Unit != "init.scope" || page.Entries[1].Message != "up to date" {
		t.Fatalf("everything kept %+v", page.Entries)
	}
	// The cursor still moves past what was left out, so following doesn't read it again.
	if !strings.Contains(page.Cursor, ";i=8;") || page.More {
		t.Fatalf("cursor %s, more %v", page.Cursor, page.More)
	}
	signIns, _ := Find(SignIns)
	page = ReadJournal(strings.NewReader(input), signIns, 100, nil)
	ids := []string{}
	for _, e := range page.Entries {
		ids = append(ids, e.Cursor[strings.Index(e.Cursor, ";i=")+3:][:1])
	}
	if !slices.Equal(ids, []string{"1", "2", "4", "6", "7", "8"}) {
		t.Fatalf("sign-ins kept lines %v", ids)
	}
	// Picked out of everything, it reads on past other lines to find its count.
	if page := ReadJournal(strings.NewReader(input), signIns, 4, nil); len(page.Entries) != 4 || !page.More {
		t.Fatalf("four sign-ins: %d, more %v", len(page.Entries), page.More)
	}
}

func TestSignInText(t *testing.T) {
	for _, l := range []string{
		"Nov 03 00:42:11 patai-0001 sshd[99]: Invalid user hunter2 from 192.168.4.177",
		"Nov 03 00:42:11 patai-0001 sudo[5]:    admin : COMMAND=/bin/true",
		"Nov  3 00:42:11 patai-0001 systemd[1]: Started Session 12 of user admin.",
		"Nov 03 00:42:11 patai-0001 systemd[1]: Started User Manager for UID 1000.",
		"Nov 03 00:42:11 patai-0001 systemd[1]: session-4.scope: Succeeded.",
		"Nov 03 00:42:11 patai-0001 systemd-logind[700]: New session 4 of user admin.",
	} {
		if !SignInText.MatchString(l) {
			t.Errorf("kept %q", l)
		}
	}
	for _, l := range []string{
		"Nov 03 00:42:11 patai-0001 systemd[1]: Started Console and API.",
		"Nov 03 00:42:11 patai-0001 patai-api[5]: store: sudo isn't a thing here",
		"    a line carried on",
	} {
		if SignInText.MatchString(l) {
			t.Errorf("dropped %q", l)
		}
	}
}

func TestReadJournalLimits(t *testing.T) {
	var input strings.Builder
	for i := 1; i <= 10; i++ {
		input.WriteString(line(i, `"SYSLOG_IDENTIFIER":"x","MESSAGE":"m"`))
	}
	all, _ := Find("all")
	page := ReadJournal(strings.NewReader(input.String()), all, 4, nil)
	if len(page.Entries) != 4 || !page.More || !strings.Contains(page.Cursor, ";i=4;") {
		t.Fatalf("got %d, more %v, cursor %s", len(page.Entries), page.More, page.Cursor)
	}
	if page := ReadJournal(strings.NewReader(input.String()), all, 10, nil); len(page.Entries) != 10 || page.More {
		t.Fatalf("all ten: got %d, more %v", len(page.Entries), page.More)
	}
	huge := line(1, `"MESSAGE":"`+strings.Repeat("é", MaxMessage)+`"`)
	page = ReadJournal(strings.NewReader(huge), all, 1, nil)
	if m := page.Entries[0].Message; len(m) > MaxMessage+10 || !strings.HasSuffix(m, " [cut]") || strings.ContainsRune(m, '�') {
		t.Fatalf("a long message wasn't cut cleanly: %d bytes", len(m))
	}
	big := strings.Repeat(line(1, `"MESSAGE":"`+strings.Repeat("x", 3000)+`"`), MaxRead/3000+10)
	if page := ReadJournal(strings.NewReader(big), all, MaxCount, nil); !page.More || len(page.Entries) >= MaxRead/3000+10 {
		t.Fatalf("read past the byte limit: %d entries", len(page.Entries))
	}
}

func TestFieldShapes(t *testing.T) {
	input := line(1, `"MESSAGE":[104,105,255]`) + line(2, `"MESSAGE":null`) + line(3, `"MESSAGE":["first","second"]`) +
		"not json\n" + `{"MESSAGE":"no cursor"}` + "\n"
	all, _ := Find("all")
	page := ReadJournal(strings.NewReader(input), all, 100, nil)
	got := []string{}
	for _, e := range page.Entries {
		got = append(got, e.Message)
	}
	if !slices.Equal(got, []string{"hi�", "", "first"}) {
		t.Fatalf("got %q", got)
	}
}

func TestRedact(t *testing.T) {
	codes := []string{"ABCDE-FGHIJ-KLMNO-PQRST"}
	for _, c := range []struct{ in, out string }{
		{"claim abcde fghij klmno pqrst now", "claim [code removed] now"},
		{"code ABCDEFGHIJKLMNOPQRST", "code [code removed]"},
		{"recovery 7ZZQ2-ABCDE-22334-QQQQQ used", "recovery [code removed] used"},
		{"Authorization: Bearer patai_" + strings.Repeat("a", 43), "Authorization: Bearer patai_[removed]"},
		{"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.e30.abc", "Authorization: Bearer [removed]"},
		{"token patai_" + strings.Repeat("b", 43) + " sent", "token patai_[removed] sent"},
		{"hash $argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA ok", "hash [hash removed] ok"},
		{"fetch https://bob:hunter2@files.example/x failed", "fetch https://[removed]@files.example/x failed"},
		{"GET /x?token=s3cretvalue&b=1", "GET /x?token=[removed]&b=1"},
		{"control-looking token: 2 '</s>'", "control-looking token: 2 '</s>'"},
		{"passwd=abc", "passwd=[removed]"},
		{`{"password": "hunter2"}`, `{"password": "[removed]"}`},
		{"key -----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n-----END EC PRIVATE KEY----- done", "key [private key removed] done"},
		{"cut -----BEGIN PRIVATE KEY-----\nMIIEv", "cut [private key removed]"},
		// What has to survive.
		{"slot 0 | n_prompt_tokens = 21, internationalization", "slot 0 | n_prompt_tokens = 21, internationalization"},
		{"-----BEGIN CERTIFICATE-----", "-----BEGIN CERTIFICATE-----"},
		{"http://192.168.4.177:8088/latest.json", "http://192.168.4.177:8088/latest.json"},
	} {
		if got := Redact(c.in, codes); got != c.out {
			t.Errorf("Redact(%q)\n got %q\nwant %q", c.in, got, c.out)
		}
	}
}

func TestReadBoots(t *testing.T) {
	out := "-1 0123456789abcdef0123456789abcdef Sun 2026-11-01 10:00:00 UTC—Sun 2026-11-01 12:30:00 UTC\n" +
		" 0 42a447525ef648fc80bf40d169b2b6de Mon 2026-11-02 23:00:31 UTC—Tue 2026-11-03 00:42:10 UTC\n" +
		"garbage\n"
	boots := ReadBoots(out)
	if len(boots) != 2 || boots[1].Index != 0 || boots[1].First != "2026-11-02T23:00:31Z" || boots[1].Last != "2026-11-03T00:42:10Z" {
		t.Fatalf("got %+v", boots)
	}
}

func frame(stream string, at time.Time, line string, partial bool) []byte {
	var b []byte
	b = append(b, 0x0a)
	b = binary.AppendUvarint(b, uint64(len(stream)))
	b = append(b, stream...)
	b = append(b, 0x10)
	b = binary.AppendUvarint(b, uint64(at.UnixNano()))
	b = append(b, 0x1a)
	b = binary.AppendUvarint(b, uint64(len(line)))
	b = append(b, line...)
	if partial {
		b = append(b, 0x20, 1)
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	return binary.BigEndian.AppendUint32(append(out, b...), uint32(len(b)))
}

func numbered(stream string, at time.Time, line string, partial bool, ordinal int) []byte {
	f := frame(stream, at, line, partial)
	body := f[4 : len(f)-4]
	meta := append([]byte{0x18}, binary.AppendUvarint(nil, uint64(ordinal))...)
	body = append(append(append([]byte{}, body...), 0x2a), binary.AppendUvarint(nil, uint64(len(meta)))...)
	body = append(body, meta...)
	out := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	return binary.BigEndian.AppendUint32(append(out, body...), uint32(len(body)))
}

func at(ns int) time.Time { return time.Date(2026, 11, 3, 0, 42, 10, ns, time.UTC) }

func localLog(frames ...[]byte) *bytes.Reader { return bytes.NewReader(bytes.Join(frames, nil)) }

func messages(p Page) []string {
	out := []string{}
	for _, e := range p.Entries {
		out = append(out, e.Message)
	}
	return out
}

func TestReadLocalLog(t *testing.T) {
	file := localLog(frame("stderr", at(100), "e1", false), frame("stdout", at(900), "o1", false),
		frame("stdout", at(1000), "o2", false), frame("stderr", at(1000), "e2", false),
		frame("stdout", at(2000), "token=abcdefghij", false))
	read := func(after string, count int) Page {
		return ReadLocalLog(file, file.Size(), time.Time{}, after, count, nil)
	}

	if p := read("", 2); !slices.Equal(messages(p), []string{"e2", "token=[removed]"}) || !p.More || p.Entries[0].Stream != "stderr" {
		t.Fatalf("newest two: %v, more %v", messages(p), p.More)
	}
	if p := read("", 10); len(p.Entries) != 5 || p.More || p.Cursor != "2026-11-03T00:42:10.000002Z#1" {
		t.Fatalf("all: %v, more %v, cursor %s", messages(p), p.More, p.Cursor)
	}
	// Following two at a time from before the first: every line once, across a page that ends inside a shared time.
	var got []string
	cursor := "2026-11-03T00:42:10Z"
	for range 4 {
		p := read(cursor, 2)
		got = append(got, messages(p)...)
		cursor = p.Cursor
	}
	if !slices.Equal(got, []string{"e1", "o1", "o2", "e2", "token=[removed]"}) || cursor != "2026-11-03T00:42:10.000002Z#1" {
		t.Fatalf("following: %v, ending at %s", got, cursor)
	}
	if p := read("2026-11-03T00:42:10.000001Z#1", 10); !slices.Equal(messages(p), []string{"e2", "token=[removed]"}) {
		t.Fatalf("after one at a shared time: %v", messages(p))
	}
	if !ValidStamp(cursor) || ValidStamp("2026-11-03T00:42:10Z#x") || ValidStamp("2026-11-03T00:42:10Z#-1") || ValidStamp("now") {
		t.Fatal("cursor shapes")
	}
}

func TestReadLocalLogOddFiles(t *testing.T) {
	// A line Docker split: its start, cut, once.
	long := localLog(frame("stdout", at(1), "first", false), frame("stdout", at(2), strings.Repeat("a", 100), true),
		frame("stdout", at(3), strings.Repeat("b", 100), true), frame("stdout", at(4), "end of it", false),
		frame("stdout", at(5), "after", false))
	p := ReadLocalLog(long, long.Size(), time.Time{}, "", 10, nil)
	if got := messages(p); len(got) != 3 || got[1] != strings.Repeat("a", 100)+" [cut]" || got[2] != "after" {
		t.Fatalf("split line: %q", got)
	}
	// A split stdout line doesn't take a stderr line with it, and Docker's numbers say which records are pieces.
	mixed := localLog(frame("stdout", at(1), "start", true), frame("stderr", at(2), "fatal error", false),
		frame("stdout", at(3), "rest", false), numbered("stdout", at(4), "second start", true, 1),
		numbered("stdout", at(5), "second rest", false, 2))
	p = ReadLocalLog(mixed, mixed.Size(), time.Time{}, "", 10, nil)
	if got := messages(p); !slices.Equal(got, []string{"start [cut]", "fatal error", "second start [cut]"}) {
		t.Fatalf("mixed: %q", got)
	}
	// Following from just after the first piece still knows the next numbered piece is one.
	if got := messages(ReadLocalLog(mixed, mixed.Size(), time.Time{}, at(4).Format(time.RFC3339Nano)+"#1", 10, nil)); len(got) != 0 {
		t.Fatalf("a piece on its own: %q", got)
	}
	// Not one of these files, or cut short at the end: nothing read past where it stops making sense.
	for _, bad := range [][]byte{[]byte("not a log at all"), append(bytes.Repeat(frame("stdout", at(1), "x", false), 3), 0xff, 0xff, 0xff, 0xff), {}} {
		r := bytes.NewReader(bad)
		if p := ReadLocalLog(r, r.Size(), time.Time{}, "", 10, nil); len(p.Entries) != 0 {
			t.Fatalf("read %v from a bad file", messages(p))
		}
	}
	// Following from further back than one read reaches says it missed some, and carries on from the oldest it has.
	var frames [][]byte
	line := strings.Repeat("x", 4000)
	for i := range MaxRead/4000 + 50 {
		frames = append(frames, frame("stdout", at(1000+i), line, false))
	}
	big := localLog(frames...)
	p = ReadLocalLog(big, big.Size(), time.Time{}, "2026-11-03T00:42:10.000000500Z", 10, nil)
	if !p.Gap || len(p.Entries) != 10 || p.Entries[0].Cursor == at(1000).Format(time.RFC3339Nano)+"#1" {
		t.Fatalf("gap %v, %d lines", p.Gap, len(p.Entries))
	}
	// Docker moved lines from after the cursor into an older file: that's a gap too, but not lines it had already.
	fresh := localLog(frame("stdout", at(500), "new file", false))
	if p := ReadLocalLog(fresh, fresh.Size(), at(400), at(100).Format(time.RFC3339Nano)+"#1", 10, nil); !p.Gap || len(p.Entries) != 1 {
		t.Fatalf("rotated past the cursor: gap %v", p.Gap)
	}
	if p := ReadLocalLog(fresh, fresh.Size(), at(400), at(450).Format(time.RFC3339Nano)+"#1", 10, nil); p.Gap || len(p.Entries) != 1 {
		t.Fatalf("rotated after the cursor: gap %v", p.Gap)
	}
}

func TestTailCursorCountsTheWholeTime(t *testing.T) {
	// Three lines at one time and a look at the newest two: following from there doesn't send the last again.
	file := localLog(frame("stdout", at(1), "before", false), frame("stdout", at(5), "A", false),
		frame("stdout", at(5), "B", false), frame("stderr", at(5), "C", false))
	tail := ReadLocalLog(file, file.Size(), time.Time{}, "", 2, nil)
	if got := messages(tail); !slices.Equal(got, []string{"B", "C"}) || tail.Cursor != at(5).Format(time.RFC3339Nano)+"#3" || !tail.More {
		t.Fatalf("tail %q at %s", got, tail.Cursor)
	}
	if got := messages(ReadLocalLog(file, file.Size(), time.Time{}, tail.Cursor, 10, nil)); len(got) != 0 {
		t.Fatalf("followed again: %q", got)
	}
}

func TestText(t *testing.T) {
	e := Entry{Time: 1793667065509104, Priority: 3, Ident: "patai-api", PID: "812", Message: "one\ntwo"}
	if got := e.Text(); got != "2026-11-03T00:51:05.509104Z patai-api[812]: error: one\n    two" {
		t.Fatalf("got %q", got)
	}
}
