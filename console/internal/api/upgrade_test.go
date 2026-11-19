package api

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
)

func TestPlainHTTPOnAnHTTPSPortIsSentToHTTPS(t *testing.T) {
	r := newRig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, "over tls") }),
		TLSConfig: r.s.TLS.Config()}
	go srv.ServeTLS(r.s.UpgradeHTTP(ln), "", "")
	t.Cleanup(func() { srv.Close() })
	addr := ln.Addr().String()

	plain := func(request string) *http.Response {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		io.WriteString(c, request)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := plain("GET /apps/?tab=keys HTTP/1.1\r\nHost: 10.0.0.5:9443\r\n\r\n")
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != "https://10.0.0.5:9443/apps/?tab=keys" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	// A name that isn't the unit's isn't sent anywhere.
	if resp := plain("GET / HTTP/1.1\r\nHost: evil.example:9443\r\n\r\n"); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatal(resp.StatusCode)
	}
	// TLS on the same port is served as before.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	got, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if string(body) != "over tls" {
		t.Fatal(string(body))
	}
}
