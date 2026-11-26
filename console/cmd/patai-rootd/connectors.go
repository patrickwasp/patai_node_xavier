package main

// Chat's connectors before a factory reset (patai_chat src/revoke-all.ts). A reset ends the key Chat seals its
// connectors' sign-ins with, so first Chat is asked to take each back at its service, while it still can. Like the
// Tailscale logout, it's best done, never a reason not to reset: a Chat that isn't running, or doesn't finish in time,
// leaves them as they were, and each ends at its service on its own.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// Chat's server, by its Compose project, and what its image runs.
	chatServer = "chat-server"
	chatEntry  = "/app/server/dist/index.js"
)

// Each service gets five seconds in Chat, all at once: this is that and Node's start, with room.
var chatRevokeLimit = 30 * time.Second

// What revoking needs, which tests stand in for.
var (
	chatContainers = func() ([]string, error) { return containersOf([]string{chatServer}, "running") }
	chatExec       = func(ctx context.Context, id string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", "exec", id, "node", chatEntry, "revoke-all").CombinedOutput()
	}
)

// chatRevokeAll asks the running Chat to take back its connectors' sign-ins, and logs what it said: only the kinds
// and names of services left for their owners to remove, never whose.
func chatRevokeAll() {
	ids, err := chatContainers()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Chat's connectors before the reset: %v\n", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), chatRevokeLimit)
	defer cancel()
	out, err := chatExec(ctx, ids[0])
	said := strings.TrimSpace(string(out))
	if len(said) > 4096 {
		said = said[len(said)-4096:]
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Chat's connectors before the reset: %v: %s\n", err, said)
		return
	}
	fmt.Fprintf(os.Stderr, "Chat's connectors before the reset: %s\n", said)
}
