// Package tailnet talks to the unit's tailscaled over its socket, through its local API: HTTP and JSON, as Tailscale's
// own CLI does, without importing Tailscale, which would tie the console to its Go version (docs/console.md, Remote
// access with Tailscale). Field names are Tailscale's own, from its ipnstate and ipn packages at the version the image
// builds. tailscaled lets the console change things once it's tailscaled's operator, which patai-rootd makes it.
package tailnet

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The Host tailscaled insists on, which no browser sends, as it refuses anything with an Origin or a Referer.
const host = "local-tailscaled.sock"

// ErrNotRunning is a socket nothing answers on: Tailscale is off, or still starting.
var ErrNotRunning = errors.New("tailscaled isn't running")

// ErrRateLimited is Let's Encrypt saying no more certificates for this name for now.
var ErrRateLimited = errors.New("too many certificates for this name lately: Let's Encrypt allows another in a day or two")

type Client struct {
	Socket string
}

// Status is what the console shows of tailscaled's: its state ("NeedsLogin", "NeedsMachineAuth", "Starting",
// "Running", "Stopped"), the login URL while it waits for one, this unit as the tailnet knows it, the tailnet, the
// names it can have a certificate for, which are there once the tailnet has HTTPS on, and what's wrong, in its words.
type Status struct {
	Version        string
	BackendState   string
	AuthURL        string
	TUN            bool
	HaveNodeKey    bool
	TailscaleIPs   []string
	Self           *Self
	Health         []string
	CurrentTailnet *Tailnet
	CertDomains    []string
}

type Self struct {
	HostName  string
	DNSName   string // with a trailing dot
	Online    bool
	KeyExpiry *time.Time // nil when its key doesn't expire
	Expired   bool
}

type Tailnet struct {
	Name            string
	MagicDNSSuffix  string
	MagicDNSEnabled bool
}

// Name is the unit's full name on its tailnet, without the trailing dot, or "".
func (s *Status) Name() string {
	if s == nil || s.Self == nil {
		return ""
	}
	return strings.TrimSuffix(s.Self.DNSName, ".")
}

// Login is how the unit joins a tailnet: at a control server, Tailscale's unless another is given, with an auth key
// if there is one. Its settings are the unit's: the tailnet's DNS and routes aren't taken, so the unit's own resolver
// and LAN stay as they are, and the console stays tailscaled's operator. Nothing updates itself: the image has no
// updater, and saying so keeps a tailnet's "auto-update new devices" from being set on it.
type Login struct {
	ControlURL string
	AuthKey    string
}

// prefs is the part of Tailscale's ipn.Prefs a login sets. The rest takes Tailscale's own defaults, as its CLI's up
// does, and stateful filtering stays off, as by default. NetfilterMode 1 is "nodivert": tailscaled makes its chains but
// doesn't jump to them first in INPUT, where it would accept anything from the tailnet ahead of patai-firewall, which
// keeps the gateway for apps on 18000 for containers alone. So the tailnet gets what the LAN does.
type prefs struct {
	ControlURL          string
	RouteAll            bool
	CorpDNS             bool
	WantRunning         bool
	NetfilterMode       int
	NoStatefulFiltering bool
	OperatorUser        string
	AutoUpdate          struct{ Check, Apply bool }
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+host+"/localapi/v0/"+path, payload)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", c.Socket)
		},
	}}
	resp, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return nil, 0, ErrNotRunning
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return data, resp.StatusCode, fmt.Errorf("tailscaled: %s", strings.TrimSpace(string(data)))
	}
	return data, resp.StatusCode, nil
}

// Status is tailscaled's, without its peers, which are the owner's other devices and none of the console's business.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	data, _, err := c.do(ctx, "GET", "status?peers=false", nil)
	if err != nil {
		return nil, err
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("tailscaled's status: %w", err)
	}
	return &s, nil
}

// Start sets the unit up to join a tailnet, as `tailscale up` would, and joins it with the auth key if there is one.
// Without one, LoginInteractive asks for a URL for the owner to sign in at.
func (c *Client) Start(ctx context.Context, login Login, operator string) error {
	p := prefs{ControlURL: login.ControlURL, WantRunning: true, NetfilterMode: 1, NoStatefulFiltering: true,
		OperatorUser: operator}
	_, _, err := c.do(ctx, "POST", "start", map[string]any{"UpdatePrefs": p, "AuthKey": login.AuthKey})
	return err
}

// LoginInteractive asks the control server for a login URL, which Status then has as AuthURL.
func (c *Client) LoginInteractive(ctx context.Context) error {
	_, _, err := c.do(ctx, "POST", "login-interactive", nil)
	return err
}

// Logout takes the unit off its tailnet: its key expires, and it would have to log in again.
func (c *Client) Logout(ctx context.Context) error {
	_, _, err := c.do(ctx, "POST", "logout", nil)
	return err
}

// Certificate is the tailnet's certificate for one of the names in CertDomains, from Let's Encrypt through Tailscale,
// whose key never leaves the unit. tailscaled keeps it, and renews it when it's asked for close to its end.
func (c *Client) Certificate(ctx context.Context, name string) (tls.Certificate, error) {
	data, code, err := c.do(ctx, "GET", "cert/"+url.PathEscape(name)+"?type=pair", nil)
	if code == http.StatusTooManyRequests {
		return tls.Certificate{}, ErrRateLimited
	}
	if err != nil {
		return tls.Certificate{}, err
	}
	// The key comes first, then the chain, in one PEM: each half is found by its type.
	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tailscaled's certificate: %w", err)
	}
	return cert, nil
}
