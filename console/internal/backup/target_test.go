package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// checkTarget is what every kind of target has to do, on an empty folder that may not be there yet. It leaves one
// backup, one+Extension, of size bytes.
func checkTarget(t *testing.T, tg Target, size int) {
	t.Helper()
	ctx := context.Background()
	if list, err := tg.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("an empty folder: %v %v", list, err)
	}
	data := bytes.Repeat([]byte("0123456789abcdef"), size/16+1)[:size]
	want := sha256.Sum256(data)
	var seen int64
	n, sum, err := tg.Upload(ctx, "one"+Extension, bytes.NewReader(data), func() error { return nil }, func(n int64) { seen = n })
	if err != nil || n != int64(len(data)) || sum != hex.EncodeToString(want[:]) || seen != n {
		t.Fatalf("upload: %d %s %d %v", n, sum, seen, err)
	}
	body, length, err := tg.Get(ctx, "one"+Extension)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil || !bytes.Equal(got, data) || length != int64(len(data)) {
		t.Fatalf("get: %d bytes of %d, length %d, %v", len(got), len(data), length, err)
	}
	if err := tg.Put(ctx, "one"+Extension+".json", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := tg.Put(ctx, "one"+Extension+".json", []byte(`{}`)); err != nil {
		t.Fatal("a small file couldn't be replaced:", err)
	}
	if got, err := ReadFile(ctx, tg, "one"+Extension+".json", 1<<10); err != nil || string(got) != "{}" {
		t.Fatalf("read %q %v", got, err)
	}
	if _, err := ReadFile(ctx, tg, "none.json", 10); err == nil {
		t.Fatal("a missing file was read")
	}
	list, err := tg.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	for _, o := range list {
		if o.Name == "one"+Extension && o.Size != int64(len(data)) || o.Modified.IsZero() {
			t.Fatalf("listed %+v", o)
		}
	}

	// A stream that turns out bad leaves nothing by its name.
	if _, _, err := tg.Upload(ctx, "two"+Extension, bytes.NewReader(data[:100]), func() error { return errors.New("bad") }, nil); err == nil {
		t.Fatal("a bad stream was finished")
	}
	if _, _, err := tg.Get(ctx, "two"+Extension); err == nil {
		t.Fatal("a bad stream left its file")
	}
	if list, _ := tg.List(ctx); len(list) != 2 {
		t.Fatalf("a bad stream left something: %+v", list)
	}
	if err := tg.Delete(ctx, "one"+Extension+".json"); err != nil {
		t.Fatal(err)
	}
	if err := Test(ctx, tg); err != nil {
		t.Fatal(err)
	}
	if list, _ := tg.List(ctx); len(list) != 1 || list[0].Name != "one"+Extension {
		t.Fatalf("after: %+v", list)
	}
}
