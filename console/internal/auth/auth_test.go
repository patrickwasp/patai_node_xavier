package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswords(t *testing.T) {
	hash := HashPassword("correct horse battery")
	if strings.Contains(hash, "correct") || !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatal(hash)
	}
	if !CheckPassword(hash, "correct horse battery") || CheckPassword(hash, "wrong") || CheckPassword("garbage", "x") {
		t.Fatal("password check is wrong")
	}
}

func TestCodes(t *testing.T) {
	code := NewCode()
	if len(code) != 23 || code != NormalizeCode(strings.ToLower(strings.ReplaceAll(code, "-", ""))) {
		t.Fatal(code)
	}
	if NewCode() == code {
		t.Fatal("codes repeat")
	}
}

func TestSessionsExpireOnTheMonotonicClock(t *testing.T) {
	s := NewSessions()
	id := s.Create(1, "ada", "owner")
	if got := s.Get(id, true); got == nil || got.Name != "ada" {
		t.Fatal(got)
	}
	s.Idle = time.Nanosecond
	time.Sleep(time.Millisecond)
	if s.Get(id, true) != nil {
		t.Fatal("an idle session lived on")
	}
}

func TestPollingDoesntKeepASessionAlive(t *testing.T) {
	s := NewSessions()
	id := s.Create(1, "ada", "owner")
	// Wide margins, as a busy builder's sleeps run long.
	s.Idle = time.Second
	time.Sleep(600 * time.Millisecond)
	if s.Get(id, false) == nil {
		t.Fatal("ended too soon")
	}
	time.Sleep(600 * time.Millisecond)
	if s.Get(id, false) != nil {
		t.Fatal("polling kept the session alive")
	}
}

func TestDeleteUserKeepsOneSession(t *testing.T) {
	s := NewSessions()
	one, two, other := s.Create(1, "a", "owner"), s.Create(1, "a", "owner"), s.Create(2, "b", "viewer")
	s.DeleteUser(1, one)
	if s.Get(one, true) == nil || s.Get(two, true) != nil || s.Get(other, true) == nil {
		t.Fatal("wrong sessions ended")
	}
}

func TestThrottle(t *testing.T) {
	th := NewThrottle()
	for range freeAttempts - 1 {
		th.Fail("k")
		if !th.Allow("k") {
			t.Fatal("throttled too soon")
		}
	}
	th.Fail("k")
	if th.Allow("k") || !th.Allow("other") {
		t.Fatal("throttle is wrong")
	}
	th.Reset("k")
	if !th.Allow("k") {
		t.Fatal("reset didn't clear")
	}
}
