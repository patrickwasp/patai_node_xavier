// patai-channel signs and checks the latest.json a channel offers (docs/updates.md, Channels).
//
//	patai-channel verify <keyring.pem>    latest.json on stdin; prints what it offers as JSON, or says why
//	                                       not and exits 1. patai-update runs it on every check.
//	patai-channel sign --key <key.pem> --cert <cert.pem> --channel <name> --version <v> --bundle <path>
//	    --manifest-hash <hash> --size <bytes> [--sequence <n>]
//	                                       prints a signed latest.json. The sequence is the time it's signed,
//	                                       unless given. scripts/sign-latest runs it, with --data, the release's
//	                                       data-formats.json, to sign what it declares of its apps' data.
//	patai-channel offer <keyring.pem> <folder> <version> <data-formats.json>
//	                                       says why the channel in folder, whose latest.json and latest-*.json are
//	                                       what it has offered, shouldn't offer the release that declares these
//	                                       (docs/updates.md, Data across slots), and exits 1. scripts/sign-latest
//	                                       runs it first.
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
	"path/filepath"
	"sort"
	"time"

	"patai/console/internal/channel"
	"patai/console/internal/formats"
)

const usage = "usage: patai-channel verify <keyring.pem> | sign --key <key.pem> --cert <cert.pem> --channel <name> " +
	"--version <v> --bundle <path> --manifest-hash <hash> --size <bytes> [--sequence <n>] [--data <data-formats.json>] | " +
	"offer <keyring.pem> <folder> <version> <data-formats.json>"

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
	dataPath := flags.String("data", "", "what the release declares of its apps' data, data-formats.json")
	flags.Parse(args)
	if *dataPath != "" {
		var err error
		if p.Data, err = readData(*dataPath); err != nil {
			die("%v", err)
		}
	}
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

func readData(path string) (formats.Release, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data formats.Release
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return data, data.Check()
}

// offer reads what a channel's folder has offered, oldest first, and says why it shouldn't offer this release.
func offer(keyring, folder, version, dataPath string) {
	pemData, err := os.ReadFile(keyring)
	if err != nil {
		die("%v", err)
	}
	roots, err := channel.Roots(pemData)
	if err != nil {
		die("%s: %v", keyring, err)
	}
	data, err := readData(dataPath)
	if err != nil {
		die("%v", err)
	}
	paths, _ := filepath.Glob(filepath.Join(folder, "latest-*.json"))
	paths = append(paths, filepath.Join(folder, "latest.json"))
	seen := map[string]bool{}
	var offers []channel.Offer
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			die("%v", err)
		}
		o, err := channel.Verify(raw, roots)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s isn't the fleet's, so it's left out: %v\n", filepath.Base(path), err)
			continue
		}
		if !seen[o.Digest] {
			seen[o.Digest] = true
			offers = append(offers, o)
		}
	}
	sort.Slice(offers, func(i, j int) bool { return offers[i].Sequence < offers[j].Sequence })
	offered := make([]formats.Offered, len(offers))
	for i, o := range offers {
		offered[i] = formats.Offered{Version: o.Version, Data: o.Data}
	}
	if refused := formats.Offer(formats.Offered{Version: version, Data: data}, offered); len(refused) > 0 {
		for _, r := range refused {
			fmt.Fprintln(os.Stderr, r.Error())
		}
		os.Exit(1)
	}
}

func main() {
	switch {
	case len(os.Args) == 3 && os.Args[1] == "verify":
		verify(os.Args[2])
	case len(os.Args) == 6 && os.Args[1] == "offer":
		offer(os.Args[2], os.Args[3], os.Args[4], os.Args[5])
	case len(os.Args) >= 2 && os.Args[1] == "sign":
		sign(os.Args[2:])
	default:
		die("%s", usage)
	}
}
