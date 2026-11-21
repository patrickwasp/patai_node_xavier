package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"patai/console/internal/channel"
	"patai/console/internal/formats"
)

// A channel's folder of signed files, as scripts/sign-latest leaves it, read back for what it has offered
// (docs/updates.md, Data across slots).
func TestOfferReadsWhatTheChannelOffered(t *testing.T) {
	caKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	key, _ := rsa.GenerateKey(rand.Reader, 4096)
	now := time.Now()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fleet"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	root, _ := x509.ParseCertificate(caDER)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "signing"},
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, root, &key.PublicKey, caKey)

	dir := t.TempDir()
	keyring := filepath.Join(dir, "keyring.pem")
	os.WriteFile(keyring, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644)
	folder := filepath.Join(dir, "channel")
	os.Mkdir(folder, 0o755)
	sign := func(sequence int64, version string, data formats.Release) []byte {
		out, err := channel.Sign(channel.Payload{Compatible: "patai-node-xavier", Channel: "stable", Sequence: sequence,
			Version: version, Bundle: version + "/patai-" + version + ".raucb", ManifestHash: strings.Repeat("ab", 32),
			Size: 1, Data: data}, key, leafDER)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(folder, fmt.Sprintf("latest-%d.json", sequence)), out, 0o644)
		return out
	}
	chat := func(version, floor int) formats.Release {
		return formats.Release{"chat-server": {Title: "Chat server", Format: &formats.Format{Version: version, Floor: floor}}}
	}
	sign(100, "2026.12.01-1", nil) // a release that declared nothing
	sign(200, "2026.12.02-1", chat(4, 3))
	os.WriteFile(filepath.Join(folder, "latest.json"), sign(300, "2026.12.03-1", chat(3, 2)), 0o644)
	os.WriteFile(filepath.Join(folder, "latest-50.json"), []byte(`{"format": "patai-latest/1"}`), 0o644)

	next := func(version, floor int) string {
		path := filepath.Join(dir, fmt.Sprintf("next-%d-%d.json", version, floor))
		data, _ := json.Marshal(chat(version, floor))
		os.WriteFile(path, data, 0o644)
		return path
	}
	for _, c := range []struct {
		version, floor int
		refused        string
	}{
		{4, 3, ""},
		{2, 1, "2026.12.02-1 made version 4"},
		{5, 5, "couldn't roll back"},
	} {
		refused, err := offer(keyring, folder, "next", next(c.version, c.floor))
		if err != nil {
			t.Fatal(err)
		}
		if c.refused == "" && len(refused) > 0 || c.refused != "" && (len(refused) == 0 || !strings.Contains(refused[0].Error(), c.refused)) {
			t.Errorf("%d/%d: %v, want %q", c.version, c.floor, refused, c.refused)
		}
	}
}
