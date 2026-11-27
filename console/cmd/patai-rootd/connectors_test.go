package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestChatRevokeAll(t *testing.T) {
	containers, exec := chatContainers, chatExec
	t.Cleanup(func() { chatContainers, chatExec = containers, exec })

	var asked []string
	chatExec = func(ctx context.Context, id string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("revoking has no time limit")
		}
		asked = append(asked, id)
		return []byte(`{"done":true,"left":0}`), nil
	}

	// No Chat running: nothing is asked, and the reset goes ahead.
	chatContainers = func() ([]string, error) { return nil, nil }
	chatRevokeAll()
	if len(asked) != 0 {
		t.Fatalf("asked %v with no Chat running", asked)
	}

	// Docker can't say: the same.
	chatContainers = func() ([]string, error) { return nil, errors.New("docker isn't running") }
	chatRevokeAll()
	if len(asked) != 0 {
		t.Fatalf("asked %v when Docker couldn't list", asked)
	}

	// Chat running: its first container is asked once.
	chatContainers = func() ([]string, error) { return []string{"c1", "c2"}, nil }
	chatRevokeAll()
	if len(asked) != 1 || asked[0] != "c1" {
		t.Fatalf("asked %v, want [c1]", asked)
	}

	// Chat failing or slow never holds the reset longer than its limit.
	chatExec = func(ctx context.Context, id string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	limit := chatRevokeLimit
	chatRevokeLimit = 200 * time.Millisecond
	t.Cleanup(func() { chatRevokeLimit = limit })
	started := time.Now()
	done := make(chan struct{})
	go func() { chatRevokeAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("revoking held the reset past its limit")
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("revoking held the reset past its limit")
	}
}
