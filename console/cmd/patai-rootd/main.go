// patai-rootd does the few things the console may ask of root (docs/console.md). It listens on a
// Unix socket, takes requests only from the patai-api account, and has a fixed list of verbs with
// checked arguments: no commands, paths or Compose input ever cross this socket.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"patai/console/internal/durable"
	"patai/console/internal/network"
	"patai/console/internal/reset"
	"patai/console/internal/resources"
	"patai/console/internal/rootd"
	"patai/console/internal/sshkeys"
	"patai/console/internal/system"
	"patai/console/internal/updates"
	"patai/console/internal/workloads"
)

const (
	workload    = "/usr/local/sbin/patai-workload"
	dataDisk    = "/usr/local/sbin/patai-data-disk"
	pataiUpdate = "/usr/local/sbin/patai-update"
	pataiNet    = "/usr/local/sbin/patai-network"
	pataiClock  = "/usr/local/sbin/patai-clock"
	pataiPower  = "/usr/local/sbin/patai-power-mode"
	apiAccount  = "patai-api"

	timerUnit   = "patai-update.service"
	installUnit = "patai-update-install.service"
	updateLock  = "/run/patai/update.lock" // patai-update holds it while it works
)

var stackName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// A person, as the gateway names one for a service run for each person: part of a hash of who they are to the app.
var personName = regexp.MustCompile(`^[0-9a-f]{16}$`)

// What an app calls one of a person's computers, such as chat:<id>.
var computerLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._-]{0,99}$`)

// A setting's or a secret's name, as patai-workload takes it.
var settingName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// unitLoad keeps the last CPU reading, so each answer covers the time since the one before.
var unitLoad resources.Reader

func main() {
	account, err := user.Lookup(apiAccount)
	if err != nil {
		log.Fatal(err)
	}
	uid, _ := strconv.Atoi(account.Uid)
	gid, _ := strconv.Atoi(account.Gid)

	if err := os.MkdirAll(filepath.Dir(rootd.Socket), 0o755); err != nil {
		log.Fatal(err)
	}
	clearUploads()
	clearBackups()
	letDisksGo()
	os.Remove(rootd.Socket)
	listener, err := net.Listen("unix", rootd.Socket)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chown(rootd.Socket, 0, gid); err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(rootd.Socket, 0o660); err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go serve(conn.(*net.UnixConn), uint32(uid))
	}
}

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	err = raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}

func serve(conn *net.UnixConn, allowed uint32) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Minute))
	uid, err := peerUID(conn)
	if err != nil || (uid != allowed && uid != 0) {
		log.Printf("refused a connection from uid %d: %v", uid, err)
		return
	}
	var req rootd.Request
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	var resp rootd.Response
	if err == nil && (req.Verb == "backup-open" || req.Verb == "restore-open" || req.Verb == "restore-check-open" ||
		req.Verb == "backup-disk-read" || req.Verb == "backup-disk-write") {
		// The backup is gathered before the answer, which can take minutes on a full disk.
		conn.SetDeadline(time.Now().Add(15 * time.Minute))
		var file *os.File
		var output string
		switch req.Verb {
		case "backup-open":
			file, output, err = openBackup(req.Backup)
		case "backup-disk-read":
			file, output, err = openDiskFile(req.Disk)
		case "backup-disk-write":
			file, output, err = createDiskFile(req.Disk)
		default:
			file, output, err = openRestore(req.Backup, req.Verb == "restore-check-open")
		}
		if err == nil {
			defer file.Close()
			if err = rootd.AnswerWithFile(conn, file, output); err != nil {
				log.Printf("%s: %v", req.Verb, err)
			}
			return
		}
		resp.Error = err.Error()
	} else if err == nil && req.Verb == "model-upload-open" {
		file, id, err := openModelUpload(req.Size)
		if err == nil {
			defer file.Close()
			if err = rootd.AnswerWithFile(conn, file, id); err != nil {
				log.Printf("model upload: %v", err)
			}
			return
		}
		resp.Error = err.Error()
	} else if err != nil {
		resp.Error = "bad request"
	} else {
		output, err := handle(req)
		resp.OK, resp.Output = err == nil, output
		if err != nil {
			resp.Error = err.Error()
		}
	}
	json.NewEncoder(conn).Encode(resp)
}

func handle(req rootd.Request) (string, error) {
	switch req.Verb {
	case "ping":
		return "pong", nil
	case "reboot":
		return whenFree(reboot)
	case "shutdown":
		return whenFree(halt)
	case "rename":
		return rename(req.Target)
	case "clock-status":
		out, err := stdout(30*time.Second, pataiClock, "status")
		return string(out), err
	case "clock-set":
		ms, err := strconv.ParseInt(req.Target, 10, 64)
		if err != nil || !system.ValidTime(ms) {
			return "", errors.New("that isn't a time")
		}
		out, err := stdout(time.Minute, pataiClock, "set", system.FormatTime(ms))
		return string(out), err
	case "power-status":
		out, err := stdout(30*time.Second, pataiPower, "status")
		return string(out), err
	case "power-mode":
		if !system.ValidPowerMode(req.Target) {
			return "", errors.New("bad power mode")
		}
		if _, err := stdout(2*time.Minute, pataiPower, req.Target); err != nil {
			return "", err
		}
		out, err := stdout(30*time.Second, pataiPower, "status")
		return string(out), err
	case "workload-start":
		if err := checkApp(req.Target); err != nil {
			return "", err
		}
		return refusal(run(4*time.Minute, workload, "start", req.Target))
	case "workload-stop":
		if err := checkApp(req.Target); err != nil {
			return "", err
		}
		return refusal(run(4*time.Minute, workload, "stop", req.Target))
	case "workload-fetch":
		if err := checkApp(req.Target); err != nil {
			return "", err
		}
		return refusal(run(30*time.Second, workload, "fetch", req.Target))
	case "workload-fetch-cancel":
		if err := checkApp(req.Target); err != nil {
			return "", err
		}
		return run(30*time.Second, workload, "cancel")
	case "app-api-on", "app-api-off":
		app, api, ok := strings.Cut(req.Target, "/")
		if !ok || checkApp(app) != nil || !stackName.MatchString(api) {
			return "", errors.New("bad API")
		}
		return refusal(run(4*time.Minute, workload, "api", strings.TrimPrefix(req.Verb, "app-api-"), app, api))
	case "apps-shown":
		out, err := stdout(30*time.Second, workload, "show")
		return string(out), err
	case "person-start", "person-stop", "person-delete", "person-list":
		// <service>/<person>/<computer>, or <service>/<person> to delete or list all of a person's computers.
		parts := strings.Split(req.Target, "/")
		whole := req.Verb == "person-delete" || req.Verb == "person-list"
		if len(parts) < 2 || len(parts) > 3 || !stackName.MatchString(parts[0]) || !personName.MatchString(parts[1]) ||
			len(parts) == 3 && !personName.MatchString(parts[2]) || len(parts) == 2 && !whole ||
			req.Verb == "person-list" && len(parts) == 3 {
			return "", errors.New("bad computer")
		}
		args := []string{"person", strings.TrimPrefix(req.Verb, "person-"), parts[0], strings.Join(parts[1:], "/")}
		if req.Verb == "person-start" {
			if req.Label != "" && !computerLabel.MatchString(req.Label) {
				return "", errors.New("bad label")
			}
			kind := "kept"
			if req.Temporary {
				kind = "temporary"
			}
			args = append(args, kind, req.Label)
		}
		out, err := stdout(2*time.Minute, workload, args...)
		return string(out), err
	case "person-sweep":
		out, err := stdout(10*time.Minute, workload, "person", "sweep")
		return string(out), err
	case "workload-status":
		return workloadStatus()
	case "model-use", "model-fetch", "model-remove":
		app, model, ok := workloads.ModelTarget(req.Target)
		if !ok || !workloads.IsService(workloads.Recipes, app) {
			return "", errors.New("bad model")
		}
		action := map[string]string{"model-use": "use", "model-fetch": "get", "model-remove": "remove"}[req.Verb]
		return refusal(run(4*time.Minute, workload, "model", action, app, model))
	case "model-add-url":
		app, model, ok := workloads.ModelTarget(req.Target)
		if !ok || !workloads.IsService(workloads.Recipes, app) || len(req.URL) > 2048 {
			return "", errors.New("bad model")
		}
		return refusal(run(30*time.Second, workload, "model", "add", app, model, req.URL))
	case "service-settings":
		// patai-workload checks each value against the recipe; this keeps what it's given to one small object.
		var values map[string]any
		if err := checkService(req.Target); err != nil {
			return "", err
		}
		if len(req.Values) > 4096 || json.Unmarshal(req.Values, &values) != nil || len(values) == 0 {
			return "", errors.New("give settings as {\"name\": value}")
		}
		return refusal(run(4*time.Minute, workload, "set", req.Target, string(req.Values)))
	case "service-secret":
		// What it is goes to patai-workload on its stdin, never as an argument, which anyone could list.
		var secret struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := checkService(req.Target); err != nil {
			return "", err
		}
		if json.Unmarshal(req.Values, &secret) != nil || !settingName.MatchString(secret.Name) || len(secret.Value) > 512 {
			return "", errors.New("bad secret")
		}
		return refusal(runInput(time.Minute, secret.Value+"\n", workload, "secret", req.Target, secret.Name))
	case "model-upload-discard":
		return "", discardModelUpload(req.Upload)
	case "model-upload-take":
		app, model, ok := workloads.ModelTarget(req.Target)
		if !ok || !workloads.IsService(workloads.Recipes, app) {
			return "", errors.New("bad model")
		}
		return takeModelUpload(req.Upload, app, model)
	case "resources":
		data, err := json.Marshal(unitLoad.Read())
		return string(data), err
	case "update-status":
		return updateStatus()
	case "update-check":
		return run(2*time.Minute, pataiUpdate, "check")
	case "update-install":
		return startUpdate("install", req.Target, req.Job)
	case "update-upload":
		return installUpload(req.Target, req.Job)
	case "update-upload-verify":
		return takeUpload()
	case "update-upload-discard":
		return "", discardUpload()
	case "update-settings":
		return "", updateSettings(req.Settings)
	case "disk-status":
		return run(30*time.Second, dataDisk, "status")
	case "disk-keep":
		return run(4*time.Minute, dataDisk, "answer-keep")
	case "disk-erase":
		return run(4*time.Minute, dataDisk, "answer-erase")
	case "factory-reset":
		return factoryReset(req.KeepData)
	case "tailscale-on":
		// Target says whether its logs go to Tailscale.
		return tailscaleStart(req.Target == "logs")
	case "tailscale-off":
		return tailscaleStop()
	case "tailscale-sync":
		return tailscaleSync()
	case "ssh-keys-list":
		keys, err := sshkeys.List(sshkeys.File)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(keys)
		return string(data), err
	case "ssh-key-add":
		key, err := sshkeys.Add(sshkeys.File, req.Key)
		if err != nil {
			return "", err
		}
		return key.Fingerprint, nil
	case "ssh-key-remove":
		return "", sshkeys.Remove(sshkeys.File, req.Key)
	case "network-status":
		out, err := stdout(30*time.Second, pataiNet, "status")
		return string(out), err
	case "network-try":
		return tryNetwork(req.Target, req.Network)
	case "network-keep", "network-undo":
		if !network.ValidID(req.Target) {
			return "", errors.New("bad change id")
		}
		_, err := stdout(time.Minute, pataiNet, strings.TrimPrefix(req.Verb, "network-"), req.Target)
		return "", err
	case "backup-finish":
		return finishBackup(req.Target)
	case "backup-disks":
		list, err := listDisks()
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(list)
		return string(data), nil
	case "backup-disk-format":
		return formatDisk(req.Disk)
	case "backup-disk-files":
		return listDiskFiles(req.Disk)
	case "backup-disk-commit", "backup-disk-abort":
		return finishDiskFile(req.Target, req.Verb == "backup-disk-commit")
	case "backup-disk-delete":
		return deleteDiskFile(req.Disk)
	case "restore-finish":
		sum := ""
		if req.Backup != nil {
			sum = req.Backup.SHA256
		}
		return finishRestore(req.Target, sum)
	case "restore-apply":
		return applyRestore(req.Backup != nil && req.Backup.KeepIdentity)
	case "restore-discard":
		backups.Lock()
		defer backups.Unlock()
		s, err := takeTurn()
		if err != nil {
			return "", err
		}
		discardRestore()
		close(s.done)
		return "", nil
	case "log-boots":
		return logBoots()
	case "logs":
		return readLogs(req.Logs)
	case "workload-logs":
		return readOutput(req.Logs)
	}
	return "", fmt.Errorf("unknown verb %q", req.Verb)
}

// reboot waits a moment, so the job that asked is recorded before the unit goes down.
func reboot() (string, error) {
	return run(30*time.Second, "systemd-run", "--on-active=2", "--unit=patai-reboot", "systemctl", "reboot")
}

// halt stops the unit for unplugging. This board's watchdog starts it again about 2 minutes after it stops
// (docs/hardware.md), so it's only off for long if someone pulls the plug.
func halt() (string, error) {
	return run(30*time.Second, "systemd-run", "--on-active=2", "--unit=patai-halt", "systemctl", "halt")
}

// rename keeps the new name on /persist and restarts. patai-identity gives the unit its name early in boot,
// so the hostname, mDNS, DHCP and the console all take it up together.
func rename(name string) (string, error) {
	if err := system.CheckName(name); err != nil {
		return "", err
	}
	// Without /persist the name would land on the slot, under where /persist mounts, and do nothing.
	if err := exec.Command("mountpoint", "-q", "/persist").Run(); err != nil {
		return "", errors.New("/persist isn't mounted, so a new name can't be kept")
	}
	return whenFree(func() (string, error) {
		if current, _ := os.Hostname(); current == name {
			return "", errors.New("that's this unit's name already")
		}
		before, readErr := os.ReadFile(system.NameFile)
		if err := os.MkdirAll(filepath.Dir(system.NameFile), 0o755); err != nil {
			return "", err
		}
		if err := durable.Write(system.NameFile, []byte(name+"\n"), 0o644); err != nil {
			return "", err
		}
		out, err := reboot()
		if err != nil {
			// The name would otherwise change at some later restart, long after this one said it hadn't.
			if readErr == nil {
				durable.Write(system.NameFile, before, 0o644)
			} else {
				os.Remove(system.NameFile)
				durable.SyncDir(filepath.Dir(system.NameFile))
			}
			return "", err
		}
		return out, nil
	})
}

// factoryReset leaves the marker and restarts: the erasing happens early in the next boot, so that an
// interrupted reset and a new one are the same thing. Its output is the next owner's claim code.
func factoryReset(keepData bool) (string, error) {
	// Without /persist the marker would land on the slot, under where /persist mounts, and do nothing.
	if err := exec.Command("mountpoint", "-q", filepath.Dir(reset.Marker)).Run(); err != nil {
		return "", fmt.Errorf("/persist isn't mounted, so only a reflash can reset this unit")
	}
	release, err := holdUpdates("the unit can't reset")
	if err != nil {
		return "", err
	}
	defer release()
	if halting() {
		return "", errors.New("the unit is shutting down")
	}
	tailscaleLogout()
	code, err := reset.Begin(reset.Marker, keepData)
	if err != nil {
		return "", err
	}
	if _, err := reboot(); err != nil {
		// A retry finds the first one's timer already waiting.
		if exec.Command("systemctl", "is-active", "--quiet", "patai-reboot.timer").Run() != nil {
			return "", err
		}
	}
	return code, nil
}

// updating is true while anything is at work on a slot. RAUC is asked too: its service carries on if
// what started it is killed.
func updating() bool {
	if updaterRunning() {
		return true
	}
	rauc, err := raucState()
	return err == nil && rauc.Operation != "idle"
}

// updaterRunning is true while patai-update runs, from the timer or for a person.
func updaterRunning() bool {
	out, _ := exec.Command("systemctl", "is-active", timerUnit, installUnit).Output()
	for _, state := range strings.Fields(string(out)) {
		if state == "active" || state == "activating" {
			return true
		}
	}
	return false
}

// rebootPending is true once a restart or a shutdown is on its way: is-active is true if either timer is.
func rebootPending() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "patai-reboot.timer", "patai-halt.timer").Run() == nil
}

func halting() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "patai-halt.timer").Run() == nil
}

// updateGate makes a look at whether an update is running and acting on the answer one step, here. The
// timer's patai-update isn't in this process, so holdUpdates takes its lock too: it either sees that run,
// or that run sees what's been done here.
var updateGate sync.Mutex

// holdUpdates keeps any update from starting until release, or says what can't happen while one runs.
func holdUpdates(what string) (release func(), err error) {
	updateGate.Lock()
	lock, err := os.OpenFile(updateLock, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		updateGate.Unlock()
		return nil, err
	}
	release = func() {
		lock.Close()
		updateGate.Unlock()
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil || updating() {
		release()
		return nil, fmt.Errorf("an update is running, so %s until it's finished", what)
	}
	return release, nil
}

// whenFree takes the unit down, unless an update is running or it's already going down.
func whenFree(down func() (string, error)) (string, error) {
	release, err := holdUpdates("the unit can't restart or shut down")
	if err != nil {
		return "", err
	}
	defer release()
	if rebootPending() {
		return "", errors.New("the unit is already restarting or shutting down")
	}
	return down()
}

func raucState() (updates.Rauc, error) {
	out, err := stdout(10*time.Second, "busctl", "--json=short", "get-property", "de.pengutronix.rauc", "/",
		"de.pengutronix.rauc.Installer", "Progress", "Operation", "Compatible")
	if err != nil {
		return updates.Rauc{}, err
	}
	return updates.ParseRauc(out)
}

var (
	slotsLock sync.Mutex
	lastSlots = []updates.Slot{}
)

// readSlots keeps the last answer, because RAUC won't give one while it's installing: it prints nothing
// and exits 0.
func readSlots() []updates.Slot {
	slotsLock.Lock()
	defer slotsLock.Unlock()
	if out, err := stdout(10*time.Second, "rauc", "status", "--detailed", "--output-format=json"); err == nil {
		if slots, err := updates.ParseSlots(out); err == nil {
			lastSlots = slots
		}
	}
	return lastSlots
}

func updateStatus() (string, error) {
	rauc, err := raucState()
	writing := err == nil && rauc.Operation != "idle"
	busy := writing || updaterRunning()
	status := updates.Status{Settings: updates.ReadSettings(updates.Provision), Busy: busy}
	status.Check, status.History = updates.ReadResults(updates.Results, busy)
	status.Upload = updates.ReadUpload(updates.Bundle, updates.BundleInfo)
	if data, err := os.ReadFile(updates.Tried); err == nil {
		status.Tried = strings.TrimSpace(string(data))
	}
	status.Slots = readSlots()
	if writing {
		status.Installing, status.Percent, status.Step = true, rauc.Percent, rauc.Step
	} else if download := updates.ReadDownload(updates.Download, nil); busy && download != nil {
		status.Installing = true
		status.Percent, status.Step = download.Step()
	}
	if busy && len(status.History) > 0 && status.History[0].State == "running" {
		status.Installing = true
	}
	data, err := json.Marshal(status)
	return string(data), err
}

// startUpdate hands the work to a service of its own and returns. An install takes longer than this
// socket stays open, and it should outlive a restart of this daemon or of the console. job is who's
// asking, so that what's left behind can be told from an earlier attempt at the same release.
func startUpdate(command, version string, job int64) (string, error) {
	if !updates.ValidVersion(version) {
		return "", errors.New("bad version")
	}
	updateGate.Lock()
	defer updateGate.Unlock()
	if updating() {
		return "", errors.New("an update is already running")
	}
	if rebootPending() {
		return "", errors.New("the unit is restarting or shutting down")
	}
	return run(30*time.Second, "systemd-run", "--quiet", "--collect", "--unit="+installUnit, pataiUpdate, command,
		version, strconv.FormatInt(job, 10))
}

var (
	// uploadLock covers everything that touches the bundle. Each request gets a goroutine of its own, and
	// the account on the far end of the socket may be racing them on purpose.
	uploadLock sync.Mutex
	// uploadTurn changes whenever the file's fate does, so an expiry already waiting for the lock can
	// tell that it's out of date. Stopping its timer is too late by then.
	uploadTurn   int
	uploadExpiry *time.Timer
)

func stopExpiry() {
	uploadTurn++
	if uploadExpiry != nil {
		uploadExpiry.Stop()
		uploadExpiry = nil
	}
}

func startExpiry() {
	turn := uploadTurn
	uploadExpiry = time.AfterFunc(updates.UploadLife, func() { expireUpload(turn) })
}

func removeUpload() {
	stopExpiry()
	os.Remove(updates.Bundle)
	os.Remove(updates.BundleInfo)
}

// takeUpload moves the file patai-api received to where that account can't reach it, then has RAUC check
// its signature.
func takeUpload() (string, error) {
	uploadLock.Lock()
	defer uploadLock.Unlock()
	if updating() {
		os.Remove(updates.Incoming)
		return "", errors.New("an update is already running")
	}
	removeUpload()
	if err := os.Rename(updates.Incoming, updates.Bundle); err != nil {
		return "", errors.New("the file didn't arrive")
	}
	data, err := checkUpload()
	if err != nil {
		removeUpload()
		return "", err
	}
	startExpiry()
	return string(data), nil
}

// checkUpload can't take what it was given to be a file. It works on an open descriptor, which follows no
// link and doesn't wait on a fifo, because root changing the owner of a name is how a link does damage.
func checkUpload() ([]byte, error) {
	file, err := os.OpenFile(updates.Bundle, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("the file didn't arrive: not a regular file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("the file didn't arrive: not a regular file")
	}
	if err := file.Chown(0, 0); err != nil {
		return nil, fmt.Errorf("the file didn't arrive: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("the file didn't arrive: %w", err)
	}
	out, err := stdout(time.Minute, "rauc", "info", "--output-format=json", updates.Bundle)
	if err != nil {
		return nil, fmt.Errorf("that isn't a signed update for this unit: %w", err)
	}
	facts, err := updates.ParseBundle(out)
	if err != nil {
		return nil, errors.New("that isn't a signed update for this unit")
	}
	if rauc, err := raucState(); err != nil || facts.Compatible != rauc.Compatible {
		return nil, fmt.Errorf("that update is for %s, not for this unit", facts.Compatible)
	}
	data, err := json.Marshal(updates.Upload{Version: facts.Version, Size: info.Size()})
	if err != nil {
		return nil, err
	}
	return data, os.WriteFile(updates.BundleInfo, data, 0o600)
}

// installUpload hands the file over to the service, which removes it. From here it isn't anyone's to
// discard: RAUC hasn't opened it yet.
func installUpload(version string, job int64) (string, error) {
	uploadLock.Lock()
	defer uploadLock.Unlock()
	if upload := updates.ReadUpload(updates.Bundle, updates.BundleInfo); upload == nil || upload.Version != version {
		return "", errors.New("that file isn't here any more, so send it again")
	}
	output, err := startUpdate("upload", version, job)
	if err == nil {
		stopExpiry()
	}
	return output, err
}

func discardUpload() error {
	uploadLock.Lock()
	defer uploadLock.Unlock()
	if updating() {
		return errors.New("an update is running, so it can't be discarded now")
	}
	removeUpload()
	return nil
}

func expireUpload(turn int) {
	uploadLock.Lock()
	defer uploadLock.Unlock()
	if turn != uploadTurn {
		return
	}
	if updating() {
		startExpiry()
		return
	}
	removeUpload()
}

var settingsLock sync.Mutex

// updateSettings forgets the last check and a failed download when the channel or its name changes: they were
// another channel's answer. A download still going keeps saying how it's doing.
func updateSettings(s *updates.Settings) error {
	if s == nil {
		return errors.New("bad request")
	}
	settingsLock.Lock()
	defer settingsLock.Unlock()
	before := updates.ReadSettings(updates.Provision)
	if err := updates.WriteSettings(updates.Provision, *s); err != nil {
		return err
	}
	if after := updates.ReadSettings(updates.Provision); after.Channel != before.Channel || after.ChannelName != before.ChannelName {
		os.Remove(filepath.Join(updates.Results, "update-check.json"))
		if fetch := workloads.ReadFetch(workloads.Progress, true); fetch != nil && fetch.Step == "failed" {
			os.Remove(workloads.Progress)
		}
	}
	return nil
}

// tryNetwork starts nothing while the unit is updating, restarting or resetting: an install from the channel
// needs the network it would change, and a restart ends a trial anyway.
func tryNetwork(port string, setting *network.Setting) (string, error) {
	if !network.ValidPort(port) || setting == nil {
		return "", errors.New("bad request")
	}
	if err := setting.Check(); err != nil {
		return "", err
	}
	updateGate.Lock()
	defer updateGate.Unlock()
	if updating() {
		return "", errors.New("an update is running, so the network can't change until it's finished")
	}
	if rebootPending() {
		return "", errors.New("the unit is restarting or shutting down")
	}
	if _, err := os.Stat(reset.Marker); err == nil {
		return "", errors.New("the unit is resetting")
	}
	data, err := json.Marshal(setting)
	if err != nil {
		return "", err
	}
	out, err := stdout(time.Minute, pataiNet, "try", port, string(data))
	return string(out), err
}

// workloadStatus asks Docker about images only with the disk ready: a question on any other state would
// have nothing to say, and costs the time of finding that out. Docker is asked once for every app's containers and
// once for its images, at the same time as patai-workload, since each command takes 75-300 ms here and the Apps
// pages wait for all of it.
func workloadStatus() (string, error) {
	status := workloads.Status{}
	var shown workloads.Shown
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		if out, err := stdout(30*time.Second, workload, "show"); err == nil {
			json.Unmarshal(out, &shown)
		}
	}()
	var disk struct {
		State string `json:"state"`
	}
	if output, err := run(30*time.Second, dataDisk, "status"); err == nil {
		json.Unmarshal([]byte(output), &disk)
	}
	status.Disk = disk.State
	var imageHere func(string) bool
	var byApp map[string][]container
	if disk.State == "ready" {
		wait.Add(1)
		go func() {
			defer wait.Done()
			byApp = containers()
		}()
		images := imagesHere()
		imageHere = func(id string) bool { return images[id] }
	}
	wait.Wait()
	status.Chosen, status.Running, status.Memory, status.People = shown.Chosen, shown.Services, shown.Memory, shown.People
	if status.Chosen == nil {
		status.Chosen = []string{}
	}
	if status.Running == nil {
		status.Running = []string{}
	}
	status.Apps = workloads.AppStatuses(workloads.AppsDir, workloads.Recipes, shown)
	status.Stacks = workloads.ReadStacks(workloads.Recipes, workloads.Data, workloads.Models, imageHere, shown.Models,
		shown.Settings, shown.Secrets, shown.Used)
	if disk.State == "ready" {
		clients := map[int]int64{}
		if data, err := os.ReadFile(resources.Clients); err == nil {
			clients = resources.ParseClientsByPID(data)
		}
		for i := range status.Stacks {
			status.Stacks[i].Services = services(status.Stacks[i].Name, clients, byApp[status.Stacks[i].Name])
		}
	}
	// patai-workload is active only while it runs: waiting to try again, it's activating.
	out, _ := exec.Command("systemctl", "is-active", "patai-fetch.service", "patai-workload.service").Output()
	status.Busy = slices.Contains(strings.Fields(string(out)), "active")
	status.Fetch = workloads.ReadFetch(workloads.Progress, status.Busy)
	data, err := json.Marshal(status)
	return string(data), err
}

// imagesHere is the ID of every image Docker has.
func imagesHere() map[string]bool {
	out, _ := stdout(30*time.Second, "docker", "image", "ls", "-a", "-q", "--no-trunc")
	here := map[string]bool{}
	for _, id := range strings.Fields(string(out)) {
		here[id] = true
	}
	return here
}

// upload is a model upload. This writes it, from a pipe whose other end patai-api has: the account never has the
// file, and gets no further than the size it gave. Take and discard name it by its id, so a request from before,
// or from before a restart of this daemon, can't act on another. Once patai-fetch has it, it's no longer here.
type upload struct {
	id    string
	pipe  *os.File
	done  chan struct{} // closed once the copy has ended, with err set
	err   error
	state string // receiving, taking, then handing it over
}

var uploads struct {
	sync.Mutex
	current *upload
}

const (
	maxModelUpload = 64 << 30
	uploadStall    = 2 * time.Minute
	// An upload nobody takes is dropped after this.
	uploadUntaken = 10 * time.Minute
	// What a model upload leaves free on the data disk, with room for patai-fetch's copy as well.
	uploadKeepFree = 1 << 30
)

func fetchRunning() bool {
	return exec.Command("systemctl", "is-active", "--quiet", "patai-fetch.service").Run() == nil
}

// clearUploads removes what an upload left when this daemon last stopped: nothing can take it now. One patai-fetch
// is copying is its own.
func clearUploads() {
	if !fetchRunning() {
		os.Remove(workloads.Upload)
	}
}

// openModelUpload starts taking an upload of size bytes into a folder only root can reach, and gives its id. Models
// are too big for /run, and patai-api can't write to /data, by its sandbox.
func openModelUpload(size int64) (*os.File, string, error) {
	if size <= 0 || size > maxModelUpload {
		return nil, "", errors.New("that's no size for a model")
	}
	uploads.Lock()
	defer uploads.Unlock()
	if uploads.current != nil {
		return nil, "", errors.New("another upload is running")
	}
	if fetchRunning() {
		return nil, "", errors.New("a download is running: wait for it first")
	}
	if err := exec.Command("mountpoint", "-q", "/data").Run(); err != nil {
		return nil, "", errors.New("the data disk isn't mounted")
	}
	// What an earlier upload left counts as free: this one replaces it.
	var disk syscall.Statfs_t
	left := regularSize(workloads.Upload)
	if err := syscall.Statfs("/data", &disk); err != nil || int64(disk.Bavail)*disk.Bsize+left-2*size < uploadKeepFree {
		return nil, "", errors.New("the data disk doesn't have room for that file")
	}
	folder := filepath.Dir(workloads.Upload)
	if err := os.Mkdir(folder, 0o700); err != nil && !os.IsExist(err) {
		return nil, "", err
	}
	info, err := os.Lstat(folder)
	if err != nil || !info.IsDir() {
		return nil, "", errors.New("the upload folder isn't a plain directory")
	}
	if err := os.Lchown(folder, 0, 0); err != nil {
		return nil, "", err
	}
	if err := os.Chmod(folder, 0o700); err != nil {
		return nil, "", err
	}
	file, err := os.OpenFile(workloads.Upload, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, "", err
	}
	read, write, err := os.Pipe()
	if err != nil {
		file.Close()
		return nil, "", err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		file.Close()
		read.Close()
		write.Close()
		return nil, "", err
	}
	u := &upload{id: hex.EncodeToString(random), pipe: read, done: make(chan struct{}), state: "receiving"}
	uploads.current = u
	go func() {
		u.err = rootd.CopyUpload(file, read, size, uploadStall)
		file.Close()
		read.Close()
		close(u.done)
		time.AfterFunc(uploadUntaken, func() { dropUpload(u, "receiving") })
	}()
	return write, u.id, nil
}

func regularSize(path string) int64 {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}

// dropUpload ends u if it's still the upload, in that state, and removes its file.
func dropUpload(u *upload, state string) bool {
	uploads.Lock()
	if uploads.current != u || u.state != state {
		uploads.Unlock()
		return false
	}
	uploads.current = nil
	uploads.Unlock()
	u.pipe.Close()
	<-u.done
	os.Remove(workloads.Upload)
	return true
}

// takeModelUpload waits for upload id to finish arriving, without holding up anything else, then hands it to
// patai-fetch, which copies and checks it. Until patai-fetch has it, no other upload can start.
func takeModelUpload(id, app, model string) (string, error) {
	uploads.Lock()
	u := uploads.current
	if u == nil || u.id != id || u.state != "receiving" {
		uploads.Unlock()
		return "", errors.New("that upload isn't here")
	}
	u.state = "taking"
	uploads.Unlock()
	select {
	case <-u.done:
	case <-time.After(uploadStall + time.Minute):
		u.pipe.Close()
		<-u.done
	}
	uploads.Lock()
	if uploads.current != u {
		uploads.Unlock()
		return "", errors.New("that upload was discarded")
	}
	if u.err != nil {
		uploads.current = nil
		uploads.Unlock()
		os.Remove(workloads.Upload)
		return "", fmt.Errorf("the file didn't arrive whole: %v", u.err)
	}
	u.state = "handing"
	uploads.Unlock()
	output, err := refusal(run(30*time.Second, workload, "model", "add-upload", app, model))
	if err != nil {
		os.Remove(workloads.Upload)
	}
	uploads.Lock()
	uploads.current = nil
	uploads.Unlock()
	return output, err
}

// discardModelUpload drops upload id, unless patai-fetch has it already.
func discardModelUpload(id string) error {
	uploads.Lock()
	u := uploads.current
	uploads.Unlock()
	if u == nil || u.id != id {
		return nil
	}
	if !dropUpload(u, "receiving") && !dropUpload(u, "taking") {
		return errors.New("that upload is being checked already")
	}
	return nil
}

// container is what Docker says about one container Compose made, as docker inspect puts it.
type container struct {
	ID     string
	Config struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Status    string
		StartedAt string
		Pid       int
		Health    *struct{ Status string }
	}
	RestartCount int
}

// containers is every container Compose made, by its project, which is its app's name. Only what Compose made: a
// container run by hand from an app's image carries its project label too. One inspect asks about them all, as JSON:
// its templates see a map rather than a container when given more than one, and fail on a key one of them lacks. If
// one has gone since the list was made, each is asked about on its own.
func containers() map[string][]container {
	out, _ := stdout(30*time.Second, "docker", "ps", "-a", "-q", "--no-trunc", "--filter", "label=com.docker.compose.oneoff=False")
	ids := strings.Fields(string(out))
	byApp := map[string][]container{}
	var found []container
	if len(ids) == 0 {
		return byApp
	}
	if facts, err := stdout(30*time.Second, "docker", append([]string{"inspect"}, ids...)...); err == nil {
		json.Unmarshal(facts, &found)
	} else {
		for _, id := range ids {
			var one []container
			if facts, err := stdout(30*time.Second, "docker", "inspect", id); err == nil && json.Unmarshal(facts, &one) == nil {
				found = append(found, one...)
			}
		}
	}
	for _, c := range found {
		if containerID.MatchString(c.ID) {
			app := c.Config.Labels["com.docker.compose.project"]
			byApp[app] = append(byApp[app], c)
		}
	}
	return byApp
}

// services reads how each of an app's services is running now, from what Docker said, its cgroup and nvmap, and its
// ready check. An app with no container has its service, absent.
func services(app string, clients map[int]int64, running []container) []workloads.Service {
	m, err := workloads.ReadManifest(workloads.Recipes, app)
	if err != nil {
		return []workloads.Service{}
	}
	found := []workloads.Service{}
	for _, c := range running {
		service := workloads.Service{Name: c.Config.Labels["com.docker.compose.service"], Title: m.Title,
			Image: c.Config.Image, State: c.State.Status, Restarts: c.RestartCount}
		if service.State == "running" {
			service.StartedAt = c.State.StartedAt
			service.Memory, service.GPUMemory = containerMemory(c.State.Pid, clients)
			// A service with no HTTP has the recipe's own health check, which Docker runs.
			healthy := answers(m.Ready) || m.Ready == "healthcheck" && c.State.Health != nil && c.State.Health.Status == "healthy"
			service.Healthy = &healthy
		}
		found = append(found, service)
	}
	if len(found) == 0 {
		found = append(found, workloads.Service{Name: "server", Title: m.Title, Image: m.Image.Ref, State: "absent"})
	}
	return found
}

// containerMemory reads a running container's own memory, resident and swapped, and nvmap's count for its
// processes, from the cgroup its first process is in, which holds every process it runs. nil is what it couldn't read.
func containerMemory(pid int, clients map[int]int64) (memory, gpuMemory *int64) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	cgroup, ok := resources.Cgroup(data)
	if pid <= 0 || err != nil || !ok {
		return nil, nil
	}
	container := resources.ContainerCgroup(cgroup)
	if data, err := os.ReadFile(filepath.Join(container, "memory.stat")); err == nil {
		own := resources.ParseMemoryStat(data)["anon"]
		if data, err := os.ReadFile(filepath.Join(container, "memory.swap.current")); err == nil {
			swap, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
			own += swap
		}
		memory = &own
	}
	if data, err := os.ReadFile(filepath.Join(cgroup, "cgroup.procs")); err == nil {
		var gpu int64
		for _, field := range strings.Fields(string(data)) {
			pid, _ := strconv.Atoi(field)
			gpu += clients[pid]
		}
		gpuMemory = &gpu
	}
	return memory, gpuMemory
}

func answers(address string) bool {
	if !strings.HasPrefix(address, "http://127.0.0.1:") {
		return false
	}
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(address)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// A service must be one the signed image carries, and one that can serve.
func checkService(name string) error {
	if !stackName.MatchString(name) {
		return fmt.Errorf("bad service name")
	}
	if !workloads.IsService(workloads.Recipes, name) {
		return fmt.Errorf("no such service: %s", name)
	}
	return nil
}

// An app must be one the signed image carries.
func checkApp(app string) error {
	if !stackName.MatchString(app) {
		return fmt.Errorf("bad app name")
	}
	if _, err := workloads.ReadApp(workloads.AppsDir, app); err != nil {
		return fmt.Errorf("no such app: %s", app)
	}
	return nil
}

// stdout is for commands whose output gets parsed, which log to stderr as they go. A failure's reason is
// the last line of that.
func stdout(limit time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return nil, errors.New(lines[len(lines)-1])
	}
	return out, nil
}

// refusal is a failure as the command put it, its last line, for an owner to read: patai-workload says why.
func refusal(output string, err error) (string, error) {
	if err == nil {
		return output, nil
	}
	if lines := strings.Split(strings.TrimSpace(output), "\n"); lines[len(lines)-1] != "" {
		return "", errors.New(lines[len(lines)-1])
	}
	return "", err
}

func run(limit time.Duration, name string, args ...string) (string, error) {
	return runInput(limit, "", name, args...)
}

// runInput is run with input on the command's stdin.
func runInput(limit time.Duration, input, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if len(output) > 4096 {
		output = output[len(output)-4096:]
	}
	if err != nil {
		return output, fmt.Errorf("%s: %v: %s", filepath.Base(name), err, output)
	}
	return output, nil
}
