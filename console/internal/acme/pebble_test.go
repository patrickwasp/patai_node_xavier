//go:build pebble

package acme

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/challenge/dns01"

	"patai/console/internal/certs"
)

// Against Pebble, Let's Encrypt's test server, with its DNS test server answering the challenges: an account is
// registered, a certificate had for a name with the node key, renewed under ARI naming the one it replaces, and
// revoked. Run with PEBBLE (its directory) and CHALLTESTSRV (its management API), -tags pebble.
func TestAgainstPebble(t *testing.T) {
	directory, chall := os.Getenv("PEBBLE"), os.Getenv("CHALLTESTSRV")
	if directory == "" || chall == "" {
		t.Skip("no Pebble")
	}
	dir := t.TempDir()
	nodeKey := &certs.KeyFile{Path: filepath.Join(dir, "node-key.pem")}
	account := &certs.KeyFile{Path: filepath.Join(dir, "acme-account.pem")}
	insecure := &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	c := &Client{
		Directory:  directory,
		Account:    func() (crypto.Signer, error) { return account.Get() },
		NodeKey:    func() (crypto.Signer, error) { return nodeKey.Get() },
		UserAgent:  "patai-console/test",
		HTTPClient: insecure,
		DNSOptions: []dns01.ChallengeOption{dns01.PropagationWait(time.Second, true)},
	}
	dns := &challtest{base: chall}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := c.Check(ctx, "patai.example.com", dns); err != nil {
		t.Fatal(err)
	}
	chain, err := c.Obtain(ctx, "patai.example.com", dns, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccountURI() == "" {
		t.Fatal("no account")
	}
	leaf, err := Leaf(chain)
	if err != nil || leaf.VerifyHostname("patai.example.com") != nil {
		t.Fatalf("%v", err)
	}
	key, _ := nodeKey.Get()
	if !key.PublicKey.Equal(leaf.PublicKey) {
		t.Fatal("not the node key")
	}
	at := c.RenewAt(ctx, chain)
	if !at.After(leaf.NotBefore) || !at.Before(leaf.NotAfter) {
		t.Fatalf("renew at %v for %v to %v", at, leaf.NotBefore, leaf.NotAfter)
	}
	renewed, err := c.Obtain(ctx, "patai.example.com", dns, chain)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Leaf(renewed)
	if again.SerialNumber.Cmp(leaf.SerialNumber) == 0 || !key.PublicKey.Equal(again.PublicKey) {
		t.Fatal("not a new certificate with the same key")
	}
	if err := c.Revoke(ctx, renewed); err != nil {
		t.Fatal(err)
	}
	// A second client with the same account key finds the same account.
	other := &Client{Directory: directory, Account: c.Account, NodeKey: c.NodeKey, HTTPClient: insecure}
	if _, err := other.client(ctx); err != nil || other.AccountURI() != c.AccountURI() {
		t.Fatalf("%v %q %q", err, other.AccountURI(), c.AccountURI())
	}
	_ = x509.Certificate{}
}

// challtest is a DNS provider that writes to pebble-challtestsrv.
type challtest struct{ base string }

func (p *challtest) post(ctx context.Context, path string, body map[string]string) error {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", p.base+path, bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (p *challtest) Present(ctx context.Context, domain, token, keyAuth string) error {
	info := dns01.GetChallengeInfo(ctx, domain, keyAuth)
	return p.post(ctx, "/set-txt", map[string]string{"host": info.EffectiveFQDN, "value": info.Value})
}

func (p *challtest) CleanUp(ctx context.Context, domain, token, keyAuth string) error {
	info := dns01.GetChallengeInfo(ctx, domain, keyAuth)
	return p.post(ctx, "/clear-txt", map[string]string{"host": info.EffectiveFQDN})
}
