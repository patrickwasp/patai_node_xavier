package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// A port that serves HTTPS gets plain HTTP too, from a browser given an address with a port and no https://, such as
// patai.local:9443. Go would answer that with "Client sent an HTTP request to an HTTPS server"; this answers it with a
// redirect to the same address over https. The first byte tells them apart: TLS starts with a handshake record, 0x16.
// Port 80 isn't one of these: it explains the unit's certificate first (plain.go).

const tlsHandshake = 0x16

// sniffFor is how long a new connection has to send its first byte.
const sniffFor = 10 * time.Second

type upgrading struct {
	net.Listener
	s     *Server
	port  string
	conns chan net.Conn
	errs  chan error
}

// UpgradeHTTP is ln, which serves TLS, with plain HTTP on it redirected to https.
func (s *Server) UpgradeHTTP(ln net.Listener) net.Listener {
	port := ""
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		port = strconv.Itoa(addr.Port)
	}
	u := &upgrading{Listener: ln, s: s, port: port, conns: make(chan net.Conn), errs: make(chan error, 1)}
	go u.accept()
	return u
}

func (u *upgrading) accept() {
	for {
		c, err := u.Listener.Accept()
		if err != nil {
			u.errs <- err
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go u.sort(c)
	}
}

// sort sends a TLS connection on to the server, and answers any other with a redirect.
func (u *upgrading) sort(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(sniffFor))
	r := bufio.NewReader(c)
	first, err := r.Peek(1)
	if err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})
	conn := &peeked{Conn: c, r: r}
	if first[0] == tlsHandshake {
		u.conns <- conn
		return
	}
	u.redirect(conn)
}

func (u *upgrading) Accept() (net.Conn, error) {
	select {
	case c := <-u.conns:
		return c, nil
	case err := <-u.errs:
		return nil, err
	}
}

// peeked is a connection whose first bytes were read to see what it is, and are read again.
type peeked struct {
	net.Conn
	r *bufio.Reader
}

func (p *peeked) Read(b []byte) (int, error) { return p.r.Read(b) }

func (u *upgrading) redirect(c *peeked) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(sniffFor))
	req, err := http.ReadRequest(c.r)
	if err != nil {
		return
	}
	resp := &http.Response{ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Connection": {"close"}, "Content-Length": {"0"}}}
	host := req.Host
	if _, _, err := net.SplitHostPort(host); err != nil && host != "" {
		host = net.JoinHostPort(host, u.port)
	}
	if host == "" || !u.s.hostAllowed(host) {
		resp.StatusCode = http.StatusMisdirectedRequest
	} else {
		resp.StatusCode = http.StatusPermanentRedirect
		resp.Header.Set("Location", "https://"+host+req.URL.RequestURI())
	}
	resp.Write(c)
}
