// patai-channel signs and checks the latest.json a channel offers (docs/updates.md, Channels).
//
//	patai-channel verify <keyring.pem>    latest.json on stdin; prints what it offers as JSON, or says why
//	                                       not and exits 1. patai-update runs it on every check.
//	patai-channel sign --key <key.pem> --cert <cert.pem> --channel <name> --version <v> --bundle <path>
//	    --manifest-hash <hash> --size <bytes> [--sequence <n>]
//	                                       prints a signed latest.json. The sequence is the time it's signed,
//	                                       unless given. scripts/sign-latest runs it.
package main

import (
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"patai/console/internal/channel"
)

const usage = "usage: patai-channel verify <keyring.pem> | sign --key <key.pem> --cert <cert.pem> --channel <name> " +
	"--version <v> --bundle <path> --manifest-hash <hash> --size <bytes> [--sequence <n>]"

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func verify(keyring string) {
	pemData, err := os.ReadFile(keyring)
	if err != nil {
		die("%v", err)
	}
	roots, err := channel.Roots(pemData)
	if err != nil {
		die("%s: %v", keyring, err)
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, channel.MaxSize+1))
	if err != nil {
		die("%v", err)
	}
	offer, err := channel.Verify(data, roots)
	if err != nil {
		die("%v", err)
	}
	out, _ := json.Marshal(offer)
	fmt.Println(string(out))
}

func readKey(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("that key can't sign")
	}
	return signer, nil
}

func readCert(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return block.Bytes, nil
}

func sign(args []string) {
	flags := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := flags.String("key", "", "the fleet signing key, PEM")
	certPath := flags.String("cert", "", "its certificate, PEM")
	var p channel.Payload
	flags.StringVar(&p.Compatible, "compatible", "patai-node-xavier", "RAUC's compatible")
	flags.StringVar(&p.Channel, "channel", "", "the channel's name")
	flags.StringVar(&p.Version, "version", "", "the release")
	flags.StringVar(&p.Bundle, "bundle", "", "the bundle, relative to the channel or a full address")
	flags.StringVar(&p.ManifestHash, "manifest-hash", "", "the bundle's manifest hash, from rauc info")
	flags.Int64Var(&p.Sequence, "sequence", time.Now().Unix(), "Unix seconds; now by default")
	flags.Int64Var(&p.Size, "size", 0, "the bundle's size in bytes")
	flags.Parse(args)
	if *keyPath == "" || *certPath == "" || p.Size < 1 || flags.NArg() > 0 {
		die("%s", usage)
	}
	key, err := readKey(*keyPath)
	if err != nil {
		die("%s: %v", *keyPath, err)
	}
	cert, err := readCert(*certPath)
	if err != nil {
		die("%s: %v", *certPath, err)
	}
	out, err := channel.Sign(p, key, cert)
	if err != nil {
		die("%v", err)
	}
	os.Stdout.Write(out)
}

func main() {
	switch {
	case len(os.Args) == 3 && os.Args[1] == "verify":
		verify(os.Args[2])
	case len(os.Args) >= 2 && os.Args[1] == "sign":
		sign(os.Args[2:])
	default:
		die("%s", usage)
	}
}
