// Package acme gets the unit a certificate from Let's Encrypt by DNS-01 at a name of the owner's own (docs/console.md,
// Remote access, Your own domain), with lego. The certificate's key is the node key, which the unit made on itself;
// the ACME account has a key of its own, as Let's Encrypt requires. Neither leaves the unit. The owner's DNS
// credentials are theirs, set on the unit and kept there (ops decisions/0009); this package only uses them.
package acme

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/acme/api"
	"github.com/go-acme/lego/v5/certificate"
	"github.com/go-acme/lego/v5/challenge"
	"github.com/go-acme/lego/v5/challenge/dns01"
	"github.com/go-acme/lego/v5/lego"
	legolog "github.com/go-acme/lego/v5/log"
	"github.com/go-acme/lego/v5/registration"
)

// LetsEncrypt is Let's Encrypt's production directory, and Staging its test one, whose certificates nothing trusts.
const (
	LetsEncrypt = lego.DirectoryURLLetsEncrypt
	Staging     = lego.DirectoryURLLetsEncryptStaging
)

// lego's own log names domains and says what it's doing, never a credential; it goes to the journal at warning and
// above, as the rest of the console's does.
func init() {
	legolog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// Issuer is what the console asks of a CA. Client is Let's Encrypt's; tests use one of their own.
type Issuer interface {
	// Check proves the owner's credentials can write a TXT record at the name's _acme-challenge, by writing one and
	// taking it away. It doesn't wait for DNS to show it.
	Check(ctx context.Context, name string, dns challenge.Provider) error
	// Obtain gets a certificate for name with the node key, by DNS-01 through dns. previous, the chain it replaces if
	// there is one, makes it a renewal under ARI, which Let's Encrypt exempts from its rate limits.
	Obtain(ctx context.Context, name string, dns challenge.Provider, previous []byte) ([]byte, error)
	// RenewAt is when the chain should be renewed: within the CA's ARI window, or at two thirds of its life.
	RenewAt(ctx context.Context, chain []byte) time.Time
	// Revoke revokes the chain, when the owner stops using the name.
	Revoke(ctx context.Context, chain []byte) error
}

// Signer gives a key the client may have had made on first use: the node key, and the account's.
type Signer func() (crypto.Signer, error)

// Client is Let's Encrypt, or another ACME directory, as the unit's own account.
type Client struct {
	Directory string
	Account   Signer // the account's key, made the first time it's needed
	NodeKey   Signer // every certificate's key
	UserAgent string

	// For tests against Pebble, Let's Encrypt's test server: a client that trusts its certificate, and DNS-01 options
	// that point its checks at Pebble's own DNS.
	HTTPClient *http.Client
	DNSOptions []dns01.ChallengeOption

	mu  sync.Mutex
	reg *acme.ExtendedAccount
}

// user is the account as lego wants it: no email, since Let's Encrypt sends none now and the unit has none to give.
type user struct {
	key crypto.Signer
	reg *acme.ExtendedAccount
}

func (u *user) GetEmail() string                       { return "" }
func (u *user) GetRegistration() *acme.ExtendedAccount { return u.reg }
func (u *user) GetPrivateKey() crypto.Signer           { return u.key }

// client is lego's, as the unit's account, registering it the first time: the owner accepted the CA's terms when they
// turned the domain on.
func (c *Client) client(ctx context.Context) (*lego.Client, error) {
	key, err := c.Account()
	if err != nil {
		return nil, fmt.Errorf("the account's key: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	u := &user{key: key, reg: c.reg}
	config := lego.NewConfig(u)
	config.CADirURL = c.Directory
	config.UserAgent = c.UserAgent
	if c.HTTPClient != nil {
		config.HTTPClient = c.HTTPClient
	}
	cl, err := lego.NewClient(config)
	if err != nil {
		return nil, err
	}
	if c.reg == nil {
		reg, err := cl.Registration.ResolveAccountByKey(ctx)
		if err != nil {
			if reg, err = cl.Registration.Register(ctx, registration.RegisterOptions{TermsOfServiceAgreed: true}); err != nil {
				return nil, fmt.Errorf("registering with the CA: %w", err)
			}
		}
		c.reg, u.reg = reg, reg
		// The account's URI is known now, so lego signs with it from here on.
		if cl, err = lego.NewClient(config); err != nil {
			return nil, err
		}
	}
	return cl, nil
}

// AccountURI is the account's, once it's registered: what a CAA record's accounturi names.
func (c *Client) AccountURI() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reg == nil {
		return ""
	}
	return c.reg.Location
}

func (c *Client) Check(ctx context.Context, name string, dns challenge.Provider) error {
	// A value no CA asked for, gone again at once: proof enough that the record can be written in that zone.
	token, keyAuth := "patai-check", "patai-check-"+fmt.Sprint(time.Now().UnixNano())
	if err := dns.Present(ctx, name, token, keyAuth); err != nil {
		return err
	}
	return dns.CleanUp(ctx, name, token, keyAuth)
}

func (c *Client) Obtain(ctx context.Context, name string, dns challenge.Provider, previous []byte) ([]byte, error) {
	cl, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	// Each name's own authoritative servers are asked, which is where the owner's provider wrote the record.
	options := append([]dns01.ChallengeOption{dns01.DisableRecursiveNSsPropagationRequirement()}, c.DNSOptions...)
	if err := cl.Challenge.SetDNS01Provider(dns, options...); err != nil {
		return nil, err
	}
	key, err := c.NodeKey()
	if err != nil {
		return nil, fmt.Errorf("the node key: %w", err)
	}
	request := certificate.ObtainRequest{Domains: []string{name}, PrivateKey: key, Bundle: true}
	if leaf, err := Leaf(previous); err == nil {
		if id, err := api.MakeARICertID(leaf); err == nil {
			request.ReplacesCertID = id
		}
	}
	res, err := cl.Certificate.Obtain(ctx, request)
	if err != nil && request.ReplacesCertID != "" && strings.Contains(err.Error(), "replaces") {
		// A certificate from another account, or one already replaced, can't be named: get a new one.
		request.ReplacesCertID = ""
		res, err = cl.Certificate.Obtain(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	return res.Certificate, nil
}

func (c *Client) RenewAt(ctx context.Context, chain []byte) time.Time {
	leaf, err := Leaf(chain)
	if err != nil {
		return time.Now()
	}
	fallback := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
	cl, err := c.client(ctx)
	if err != nil {
		return fallback
	}
	info, err := cl.Certificate.GetRenewalInfo(ctx, leaf)
	if err != nil || info == nil || info.ExtendedRenewalInfo == nil {
		return fallback
	}
	window := info.SuggestedWindow
	if window.Start.IsZero() || window.End.Before(window.Start) {
		return fallback
	}
	// A time within the window, as ARI asks, without waking at a moment every unit shares.
	return window.Start.Add(window.End.Sub(window.Start) / 3)
}

func (c *Client) Revoke(ctx context.Context, chain []byte) error {
	cl, err := c.client(ctx)
	if err != nil {
		return err
	}
	return cl.Certificate.Revoke(ctx, chain)
}

// Leaf is the first certificate of a PEM chain.
func Leaf(chain []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(chain)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
