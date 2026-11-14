// patai-console is for someone with root on the unit, over SSH or on the serial console.
//
//	patai-console reset-owner <name>     a temporary password, for an owner with no recovery codes left
//	patai-console new-claim              a claim code, for a unit with no owner
//	patai-console factory-reset <unit-name> [--keep-data]
//	                                      erase this unit for its next owner, and restart it
//
// It runs as the patai-api account, so the files it touches stay that account's. A factory reset is
// the exception: it asks patai-rootd, like the console's own button.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"patai/console/internal/auth"
	"patai/console/internal/rootd"
	"patai/console/internal/store"
)

const dir = "/persist/console"

const usage = "usage: patai-console reset-owner <name> | new-claim | factory-reset <unit-name> [--keep-data]"

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func dropToAPI() {
	u, err := user.Lookup("patai-api")
	if err != nil {
		die("%v", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if err := syscall.Setgroups([]int{gid}); err != nil {
		die("run this as root: %v", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		die("%v", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		die("%v", err)
	}
}

func audit(action, target string) store.Audit {
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return store.Audit{BootID: string(boot), Actor: "root", Origin: "local", Source: "console", Action: action, Target: target, Result: "ok"}
}

func main() {
	if len(os.Args) < 2 {
		die(usage)
	}
	switch os.Args[1] {
	case "reset-owner":
		if len(os.Args) != 3 {
			die("usage: patai-console reset-owner <name>")
		}
		dropToAPIThen(func(s *store.Store) {
			u, err := s.UserByName(os.Args[2])
			if err != nil {
				die("no such user: %s", os.Args[2])
			}
			password := auth.NewCode()
			if err := s.SetPassword(u.ID, auth.HashPassword(password), audit("reset-owner", u.Name)); err != nil {
				die("%v", err)
			}
			fmt.Printf("Temporary password for %s: %s\nChange it after logging in.\n", u.Name, password)
		})
	case "new-claim":
		dropToAPIThen(func(s *store.Store) {
			claimed, err := s.HasOwner()
			if err != nil {
				die("%v", err)
			}
			if claimed {
				die("this unit has an owner; a claim is for a unit with none")
			}
			code := auth.NewCode()
			if err := os.WriteFile(filepath.Join(dir, "claim-code"), []byte(code+"\n"), 0o600); err != nil {
				die("%v", err)
			}
			fmt.Printf("Claim code: %s\n", code)
		})
	case "factory-reset":
		factoryReset(os.Args[2:])
	default:
		die(usage)
	}
}

// factoryReset wants the unit's name, because it can't be undone and there's no one to ask.
func factoryReset(args []string) {
	if len(args) == 0 || len(args) > 2 || (len(args) == 2 && args[1] != "--keep-data") {
		die(usage)
	}
	name, err := os.Hostname()
	if err != nil {
		die("%v", err)
	}
	if args[0] != name {
		die("this unit is %s", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := rootd.Client{Path: rootd.Socket}
	resp, err := client.Call(ctx, rootd.Request{Verb: "factory-reset", KeepData: len(args) == 2})
	if err != nil {
		die("%v", err)
	}
	fmt.Printf("Claim code for the next owner: %s\nThe unit is restarting, and this connection will end.\n", resp.Output)
}

// dropToAPIThen does the store work unprivileged, in a child, so the parent stays root to restart the
// daemon and end its sessions.
func dropToAPIThen(work func(*store.Store)) {
	if os.Getenv("PATAI_CONSOLE_CHILD") == "" {
		self, err := os.Executable()
		if err != nil {
			die("%v", err)
		}
		child := exec.Command(self, os.Args[1:]...)
		child.Env = append(os.Environ(), "PATAI_CONSOLE_CHILD=1")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Run(); err != nil {
			os.Exit(1)
		}
		if err := exec.Command("systemctl", "restart", "patai-api").Run(); err != nil {
			die("restart patai-api: %v", err)
		}
		return
	}
	dropToAPI()
	s, err := store.Open(dir)
	if err != nil {
		die("%v", err)
	}
	defer s.Close()
	work(s)
}
