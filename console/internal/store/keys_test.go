package store

import (
	"testing"
	"time"
)

func TestKeysScopeAndIDs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	free, err := s.CreateKey(Token{Name: "free"}, "h1", Limits{}, Audit{})
	if err != nil || free.Scope != KeyScope {
		t.Fatalf("%+v %v", free, err)
	}
	held, err := s.CreateKey(Token{Name: "held"}, "h2", Limits{TokensPerDay: 5}, Audit{})
	if err != nil || held.Scope != KeyScope {
		t.Fatalf("%+v %v", held, err)
	}
	if err := s.SetLimits(free.ID, Limits{APIs: []string{"chat"}}, Audit{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TokenByHash("h1"); got.Scope != KeyScope {
		t.Fatalf("%+v", got)
	}
	// A new key never takes a deleted one's id, and with it its usage.
	s.AddAPIRequests([]APIRequest{{At: time.Now().UTC().Format(time.RFC3339Nano), KeyID: held.ID, Key: "held", PromptTokens: 9}})
	if err := s.DeleteToken(held.ID, Audit{}); err != nil {
		t.Fatal(err)
	}
	next, err := s.CreateKey(Token{Name: "next"}, "h3", Limits{}, Audit{})
	if err != nil || next.ID <= held.ID {
		t.Fatalf("%+v %v", next, err)
	}
	if tokens, _, _ := s.UsedOn(Day(time.Now()), next.ID); tokens != 0 {
		t.Fatalf("inherited %d tokens", tokens)
	}
	keys, _ := s.Keys()
	if len(keys) != 2 {
		t.Fatalf("%+v", keys)
	}
}

func TestDailyTotalsOutliveTheLog(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	s.AddAPIRequests([]APIRequest{{At: at, KeyID: 7, Key: "k", PromptTokens: 10, CompletionTokens: 5, Status: 200},
		{At: at, KeyID: 7, Key: "k", Status: 429}, {At: at, KeyID: 7, Key: "k", Images: 2, Status: 200}})
	// A flood of other lines pushes these out of the log.
	if _, err := s.db.Exec(`DELETE FROM api_requests`); err != nil {
		t.Fatal(err)
	}
	tokens, images, err := s.UsedOn(Day(time.Now()), 7)
	if err != nil || tokens != 15 || images != 2 {
		t.Fatalf("%d %d %v", tokens, images, err)
	}
	usage, _ := s.UsageSince(Day(time.Now()))
	if len(usage) != 1 || usage[0].Requests != 3 || usage[0].Errors != 1 {
		t.Fatalf("%+v", usage)
	}
}
