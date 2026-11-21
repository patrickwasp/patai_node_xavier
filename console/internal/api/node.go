package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"patai/console/internal/certs"
	"patai/console/internal/durable"
	"patai/console/internal/rootd"
)

var nodeIDForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// LoadNodeID is the unit's id for the phones that connect to it (Chat's Connect phone): a random UUID made the first time
// the console starts, in dir. It's in every backup, as everything there is, so a restore gives a unit the id the phones
// know, unless it's restored as a copy of one still running, which keeps its own (patai-rootd, restore). A factory reset
// makes a new one. Random and nothing else, because anyone may read it: Chat's health check says it with no sign-in.
func LoadNodeID(dir string) (string, error) {
	path := filepath.Join(dir, "node-id")
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); nodeIDForm.MatchString(id) {
			return id, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return id, durable.Write(path, []byte(id+"\n"), 0o644)
}

// nodeInfo is what the gateway for apps tells an app about the unit, for a phone to find it and trust it (Chat's Connect
// phone): its id, its name, its own certificate's fingerprint as browsers show it, the pins of its keys, and where a
// phone may reach it, the LAN's names and addresses, then the tailnet's. Only that, and only to an app's key.
//
// keys.unit is the pin of the unit's own key, at its LAN names and addresses; keys.node, once the unit has made its node
// key, is the pin of the key every certificate it gets by ACME has (docs/console.md, Remote access, The node key).
func (s *Server) nodeInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	addresses := []string{s.Hostname + ".local"}
	if resp, err := s.Rootd.Call(ctx, rootd.Request{Verb: "network-status"}); err == nil {
		var status struct {
			Ports []struct {
				Addresses []string `json:"addresses"`
			} `json:"ports"`
		}
		if json.Unmarshal([]byte(resp.Output), &status) == nil {
			for _, port := range status.Ports {
				for _, address := range port.Addresses {
					if ip, _, err := net.ParseCIDR(address); err == nil {
						addresses = append(addresses, ip.String())
					}
				}
			}
		}
	}
	if s.tailscaleAvailable() && s.tailscaleOn() {
		if st, err := s.tailscaleClient().Status(ctx); err == nil && st.Name() != "" {
			addresses = append(addresses, st.Name())
		}
	}
	keys := map[string]string{"unit": s.UnitPin}
	if pin := s.nodePin(); pin != "" {
		keys["node"] = pin
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": s.NodeID, "name": s.Hostname, "fingerprint": s.Fingerprint, "keys": keys, "addresses": addresses,
	})
}

// nodePin is the node key's pin, or "" while the unit has none.
func (s *Server) nodePin() string {
	if s.NodeKey == nil {
		return ""
	}
	key := s.NodeKey.Existing()
	if key == nil {
		return ""
	}
	pin, err := certs.Pin(key.Public())
	if err != nil {
		return ""
	}
	return pin
}
