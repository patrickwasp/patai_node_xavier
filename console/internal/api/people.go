package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"patai/console/internal/rootd"
)

// A person's own container of a service run for each person, such as Chat's terminal (docs/apps.md, The Chat app):
// the app's own server asks for it through the gateway for apps, with the app's key, saying whose it is in X-User-Id.
// Only the app holds that key, so the header is the app's word. The gateway has patai-rootd start that person's
// container when they first use it, sends each request on with the container's own key in place of the app's, and stops
// it once it has gone unused for personIdle. So the app's key never reaches a container a person can run commands in.

const (
	personIdle  = 30 * time.Minute
	personStart = 2 * time.Minute
	// The longest sign-in message a WebSocket's first frame may be.
	signInMax = 64 << 10
)

type person struct {
	target *url.URL // nil until it's known to run
	key    string
	used   time.Time
	open   int // requests going on, a WebSocket among them
}

type people struct {
	mu     sync.Mutex
	byName map[string]*person // by <service>/<person>
	reaper sync.Once
	same   map[string][]byte // answers that are the same in everyone's container, by <service><path>
}

// What Open WebUI asks a terminal before each chat, to tell the model what it can do, and gives up on after 3 s: less
// than a container takes to start. Each person's says the same, since their hosts are named the same, so the first
// answer is kept, and given at once while a person's own starts.
var sameForEveryone = map[string]bool{"/api/config": true, "/system": true}

// personOf is who a request is for, as the service's containers are named: part of a hash of the app and the app's
// name for them, so it says nothing about them and can't be a path.
func personOf(app string, r *http.Request) (string, bool) {
	id := r.Header.Get("X-User-Id")
	if id == "" || len(id) > 256 {
		return "", false
	}
	sum := sha256.Sum256([]byte(app + "\x00" + id))
	return hex.EncodeToString(sum[:8]), true
}

// openPerson has the person's container started if it isn't known to run, and counts a request to it until done.
func (s *Server) openPerson(ctx context.Context, service, who string) (target *url.URL, key string, done func(), err error) {
	name := service + "/" + who
	s.persons.mu.Lock()
	p := s.persons.byName[name]
	if p != nil && p.target != nil {
		target, key = p.target, p.key
	}
	s.persons.mu.Unlock()
	if target == nil {
		ctx, cancel := context.WithTimeout(ctx, personStart)
		defer cancel()
		resp, err := s.Rootd.Call(ctx, rootd.Request{Verb: "person-start", Target: name})
		if err != nil {
			return nil, "", nil, err
		}
		var started struct{ Address, Key string }
		if err := json.Unmarshal([]byte(resp.Output), &started); err != nil {
			return nil, "", nil, errors.New("the unit couldn't say where it is")
		}
		target, err = url.Parse(started.Address)
		if err != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" || started.Key == "" {
			return nil, "", nil, errors.New("the unit couldn't say where it is")
		}
		key = started.Key
	}
	s.persons.mu.Lock()
	if s.persons.byName == nil {
		s.persons.byName = map[string]*person{}
	}
	if p = s.persons.byName[name]; p == nil {
		p = &person{}
		s.persons.byName[name] = p
	}
	p.target, p.key = target, key
	p.open++
	p.used = time.Now()
	s.persons.mu.Unlock()
	return target, key, func() {
		s.persons.mu.Lock()
		p.open--
		p.used = time.Now()
		s.persons.mu.Unlock()
	}, nil
}

// forgetPerson is for a container that didn't answer: the next request has it started again.
func (s *Server) forgetPerson(name string) {
	s.persons.mu.Lock()
	if p := s.persons.byName[name]; p != nil {
		p.target = nil
	}
	s.persons.mu.Unlock()
}

// personHTTP sends a request with the app's key to the person's container, under the route's prefix. What it can do,
// its openapi.json, is the same for everyone, and the app asks for it without saying for whom: it's the copy the
// recipe ships, from the image it pins, so no container starts for it.
func (s *Server) personHTTP(w http.ResponseWriter, r *http.Request, app, service, prefix string) {
	if r.URL.Path == prefix+"openapi.json" && r.Method == http.MethodGet {
		spec, err := os.ReadFile(filepath.Join(s.RecipesPath, service, "openapi.json"))
		if err != nil {
			apiError(w, http.StatusNotFound, "invalid_request_error", "unknown_url", "it doesn't say what it can do")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(spec)
		return
	}
	who, ok := personOf(app, r)
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "no_person", "say whose it is in X-User-Id")
		return
	}
	base := strings.TrimSuffix(prefix, "/")
	path := strings.TrimPrefix(r.URL.Path, base)
	same := r.Method == http.MethodGet && sameForEveryone[path]
	if same {
		s.persons.mu.Lock()
		kept := s.persons.same[service+path]
		s.persons.mu.Unlock()
		if kept != nil {
			go func() {
				if _, _, done, err := s.openPerson(context.Background(), service, who); err == nil {
					done()
				}
			}()
			w.Header().Set("Content-Type", "application/json")
			w.Write(kept)
			return
		}
	}
	target, key, done, err := s.openPerson(r.Context(), service, who)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "server_error", "unavailable", err.Error())
		return
	}
	defer done()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.Header.Set("Authorization", "Bearer "+key)
		},
		ModifyResponse: func(resp *http.Response) error {
			if !same || resp.StatusCode != http.StatusOK {
				return nil
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, signInMax))
			resp.Body.Close()
			if err != nil {
				return err
			}
			if json.Valid(body) {
				s.persons.mu.Lock()
				if s.persons.same == nil {
					s.persons.same = map[string][]byte{}
				}
				s.persons.same[service+path] = body
				s.persons.mu.Unlock()
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength, resp.Header["Content-Length"] = int64(len(body)), nil
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.forgetPerson(service + "/" + who)
			apiError(w, http.StatusBadGateway, "server_error", "unavailable", "it isn't answering")
		},
	}
	proxy.ServeHTTP(w, r)
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// personSocket is a WebSocket to a person's container. Open WebUI signs in with its first message, {"token": key}, not a
// header, so the gateway answers the handshake itself, with no extension, so that frames pass as they are, and reads that
// message. With the app's key, it opens one of its own to the person's container and sends the message on with the
// container's key in its place. Then it passes frames both ways.
func (s *Server) personSocket(w http.ResponseWriter, r *http.Request, v *view) {
	challenge := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || challenge == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "not a WebSocket", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "not a WebSocket", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n", accept(challenge))
	if buffered.Flush() != nil {
		return
	}
	client.SetReadDeadline(time.Now().Add(30 * time.Second))
	message, err := readText(buffered.Reader)
	if err != nil {
		return
	}
	var signIn map[string]any
	if json.Unmarshal(message, &signIn) != nil {
		return
	}
	token, _ := signIn["token"].(string)
	sum := sha256.Sum256([]byte(token))
	app, ok := v.keys[hex.EncodeToString(sum[:])]
	if !ok || token == "" {
		return
	}
	service, prefix, ok := find(v.people[app], r.URL.Path)
	if !ok {
		return
	}
	who, ok := personOf(app, r)
	if !ok {
		return
	}
	target, key, done, err := s.openPerson(context.Background(), service, who)
	if err != nil {
		return
	}
	defer done()
	upstream, err := net.DialTimeout("tcp", target.Host, 10*time.Second)
	if err != nil {
		s.forgetPerson(service + "/" + who)
		return
	}
	defer upstream.Close()
	ours := make([]byte, 16)
	rand.Read(ours)
	challenge = base64.StdEncoding.EncodeToString(ours)
	path := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(prefix, "/"))
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", path, target.Host, challenge)
	// Who it's for and in which chat, as Open WebUI said: its own headers, never a credential.
	for name, values := range r.Header {
		if strings.HasPrefix(name, "X-") && len(values) > 0 && !strings.ContainsAny(values[0], "\r\n") {
			request += name + ": " + values[0] + "\r\n"
		}
	}
	upstream.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(upstream, request+"\r\n"); err != nil {
		return
	}
	reader := bufio.NewReader(upstream)
	answer, err := http.ReadResponse(reader, nil)
	if err != nil || answer.StatusCode != http.StatusSwitchingProtocols || answer.Header.Get("Sec-WebSocket-Accept") != accept(challenge) {
		return
	}
	signIn["token"] = key
	message, _ = json.Marshal(signIn)
	if writeText(upstream, message) != nil {
		return
	}
	client.SetReadDeadline(time.Time{})
	upstream.SetDeadline(time.Time{})
	finished := make(chan struct{}, 2)
	go func() {
		io.Copy(upstream, buffered.Reader)
		finished <- struct{}{}
	}()
	go func() {
		io.Copy(client, reader)
		finished <- struct{}{}
	}()
	<-finished
}

// accept is the answer to a WebSocket handshake's challenge (RFC 6455, 4.2.2).
func accept(challenge string) string {
	sum := sha1.Sum([]byte(challenge + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// readText reads a client's first frame, which has to be one masked text frame, and unmasks it.
func readText(r *bufio.Reader) ([]byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	size := uint64(head[1] & 0x7f)
	switch size {
	case 126:
		var n [2]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return nil, err
		}
		size = uint64(binary.BigEndian.Uint16(n[:]))
	case 127:
		var n [8]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return nil, err
		}
		size = binary.BigEndian.Uint64(n[:])
	}
	if head[0] != 0x81 || head[1]&0x80 == 0 || size > signInMax {
		return nil, errors.New("not a sign-in")
	}
	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return nil, err
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return payload, nil
}

// writeText writes one text frame as a client does, masked.
func writeText(w io.Writer, payload []byte) error {
	frame := []byte{0x81}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 1<<16:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := w.Write(frame)
	return err
}

// reapPeople stops each person's container once it has gone unused for personIdle. One running that the gateway hasn't
// seen, as after the console restarts, starts its time when it's found.
func (s *Server) reapPeople() {
	for range time.Tick(time.Minute) {
		if v, err := s.view(context.Background()); err == nil {
			s.persons.mu.Lock()
			if s.persons.byName == nil {
				s.persons.byName = map[string]*person{}
			}
			for service, list := range v.running {
				for _, who := range list {
					if name := service + "/" + who; s.persons.byName[name] == nil {
						s.persons.byName[name] = &person{used: time.Now()}
					}
				}
			}
			s.persons.mu.Unlock()
		}
		var idle []string
		s.persons.mu.Lock()
		for name, p := range s.persons.byName {
			if p.open == 0 && time.Since(p.used) > personIdle {
				idle = append(idle, name)
				delete(s.persons.byName, name)
			}
		}
		s.persons.mu.Unlock()
		for _, name := range idle {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if _, err := s.Rootd.Call(ctx, rootd.Request{Verb: "person-stop", Target: name}); err != nil {
				log.Printf("stopping %s: %v", name, err)
			}
			cancel()
		}
	}
}
