package diag

import (
	"encoding/binary"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A workload's output is read from Docker's own file for it, not asked of Docker: Docker 26 writes a warning to the
// journal for every request for a container's logs, and a request could start Docker again after someone stopped it.
// The local log driver keeps each line as a protobuf LogEntry with its length before and after, so the file reads
// from either end.

// Output's cursor is the time Docker gave the last line sent, to the nanosecond, and how many lines with exactly
// that time have been sent, since stdout and stderr can share one: "2026-11-03T00:42:10.123456789Z#1".
type outputCursor struct {
	at   time.Time
	seen int
}

func parseOutputCursor(s string) (outputCursor, bool) {
	stamp, count, _ := strings.Cut(s, "#")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || len(s) > 48 {
		return outputCursor{}, false
	}
	seen := 0
	if count != "" {
		if seen, err = strconv.Atoi(count); err != nil || seen < 0 || seen > 1_000_000 {
			return outputCursor{}, false
		}
	}
	return outputCursor{at, seen}, true
}

// ValidStamp is a cursor into a workload's output.
func ValidStamp(s string) bool {
	_, ok := parseOutputCursor(s)
	return ok
}

// maxRecord is far past the 16 KiB Docker splits a line at, so a bigger length means the file isn't one of these.
const maxRecord = 1 << 20

type record struct {
	stream  string
	at      time.Time
	line    []byte
	partial bool
	ordinal uint64 // the piece of a line Docker split, from 1, if it says
}

// decodeRecord reads a LogEntry: source = 1, time_nano = 2, line = 3, partial = 4, and from its partial-line
// metadata, 5, the ordinal, 3. Anything else is passed over.
func decodeRecord(b []byte) (record, bool) {
	var r record
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return r, false
		}
		b = b[n:]
		field, wire := key>>3, key&7
		switch wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return r, false
			}
			b = b[n:]
			switch field {
			case 2:
				r.at = time.Unix(0, int64(v)).UTC()
			case 4:
				r.partial = v != 0
			}
		case 2:
			size, n := binary.Uvarint(b)
			if n <= 0 || size > uint64(len(b)-n) {
				return r, false
			}
			value := b[n : n+int(size)]
			b = b[n+int(size):]
			switch field {
			case 1:
				r.stream = string(value)
			case 3:
				r.line = value
			case 5:
				r.ordinal = ordinal(value)
			}
		case 1:
			if len(b) < 8 {
				return r, false
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return r, false
			}
			b = b[4:]
		default:
			return r, false
		}
	}
	return r, !r.at.IsZero()
}

// ordinal is field 3 of PartialLogEntryMetadata, or 0.
func ordinal(b []byte) uint64 {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return 0
		}
		b = b[n:]
		switch key & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return 0
			}
			b = b[n:]
			if key>>3 == 3 {
				return v
			}
		case 2:
			size, n := binary.Uvarint(b)
			if n <= 0 || size > uint64(len(b)-n) {
				return 0
			}
			b = b[n+int(size):]
		default:
			return 0
		}
	}
	return 0
}

// ReadLocalLog reads a local-driver log file back from its end, reading at most MaxRead bytes: the newest count
// lines, or the lines after the cursor, up to count of them, oldest first either way. rotated is when Docker last
// moved lines out to an older file, or zero.
//
// Following that can't reach back to its cursor says so with Gap, and carries on from the oldest line it reached:
// the cursor may be further back than a read reaches, or in a file Docker has moved aside since.
func ReadLocalLog(f io.ReaderAt, size int64, rotated time.Time, after string, count int, codes []string) Page {
	page := Page{Entries: []Entry{}, Cursor: after}
	var from outputCursor
	if after != "" {
		from, _ = parseOutputCursor(after)
	}
	var records []record // newest first, until they're turned round
	var read int64
	length := make([]byte, 4)
	pos := size
	reached := false // a line from before the cursor, or before the oldest one kept and its time
	for pos >= 8 {
		// Not following, it reads on past count while lines share the oldest one's time, so that its cursor counts
		// them all.
		if after == "" && len(records) > count && !records[len(records)-1].at.Equal(records[count-1].at) {
			reached = true
			break
		}
		if _, err := f.ReadAt(length, pos-4); err != nil {
			break
		}
		n := int64(binary.BigEndian.Uint32(length))
		if n > maxRecord || n+8 > pos {
			break
		}
		if read+n > MaxRead {
			page.Gap = after != ""
			break
		}
		body := make([]byte, n)
		if _, err := f.ReadAt(body, pos-4-n); err != nil {
			break
		}
		r, ok := decodeRecord(body)
		if !ok {
			break
		}
		read += n + 8
		pos -= n + 8
		if after != "" && r.at.Before(from.at) {
			reached = true
			break
		}
		records = append(records, r)
	}
	if after != "" && !reached && pos < 8 && !rotated.IsZero() && from.at.Before(rotated) {
		page.Gap = true
	}
	slices.Reverse(records)
	// Each line's cursor counts the lines at its time from the first of them, this one included. The records start
	// at the first line with the cursor's time, or the oldest one kept's, unless a limit or the file's start came
	// first. The cursor's own count says how many of its time were sent already.
	var at time.Time
	seen, skip := 0, 0
	if after != "" && !page.Gap {
		skip = from.seen
	}
	first := 0
	if after == "" && len(records) > count {
		first = len(records) - count
		page.More = true
	} else if after == "" {
		page.More = pos >= 8 || !rotated.IsZero()
	}
	// A line Docker split goes on in the records after its first, which are left out: the page shows the start of a
	// line anyway. Docker numbers the pieces. Without numbers, a piece follows a partial record of its own stream.
	continuing := map[string]bool{}
	sent := 0
	for i, r := range records {
		if r.at.Equal(at) {
			seen++
		} else {
			at, seen = r.at, 1
		}
		piece := r.ordinal > 1 || r.ordinal == 0 && continuing[r.stream]
		continuing[r.stream] = r.partial
		if i < first || after != "" && r.at.Equal(from.at) && seen <= skip {
			continue
		}
		if after != "" && sent == count {
			page.More = true
			break
		}
		page.Cursor = r.at.Format(time.RFC3339Nano) + "#" + strconv.Itoa(seen)
		sent++
		if piece {
			continue
		}
		message := strings.ToValidUTF8(strings.TrimRight(string(r.line), "\r\n"), "\uFFFD")
		if r.partial && len(message) <= MaxMessage {
			message += " [cut]"
		}
		page.Entries = append(page.Entries, Entry{Cursor: page.Cursor, Time: r.at.UnixMicro(), Priority: 6,
			Stream: r.stream, Message: cut(Redact(message, codes))})
	}
	return page
}
