// Package network is the console's side of the owner's network settings (docs/console.md, Network
// settings). patai-network on the unit checks a setting in full and makes the change. This keeps what
// crosses the root helper's socket to the shape it should have, and says it in a line for the audit log.
package network

import (
	"errors"
	"regexp"
	"strings"
)

// Setting is one port's: DHCP, or a static IPv4 address with its prefix length.
type Setting struct {
	Mode    string   `json:"mode"`
	Address string   `json:"address,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

var (
	portName = regexp.MustCompile(`^gbe[1-9]$`)
	changeID = regexp.MustCompile(`^[0-9a-f]{16}$`)
	addrText = regexp.MustCompile(`^[0-9./]{0,18}$`)
)

func ValidPort(port string) bool { return portName.MatchString(port) }

func ValidID(id string) bool { return changeID.MatchString(id) }

// Label is what's printed beside the port on the board.
func Label(port string) string { return "GbE " + strings.TrimPrefix(port, "gbe") }

// Check is the shape only: the unit decides what's an address it can use.
func (s Setting) Check() error {
	if s.Mode != "dhcp" && s.Mode != "static" {
		return errors.New("a port is either DHCP or static")
	}
	if len(s.DNS) > 3 {
		return errors.New("give at most 3 DNS servers")
	}
	for _, text := range append([]string{s.Address, s.Gateway}, s.DNS...) {
		if !addrText.MatchString(text) {
			return errors.New("an address is digits and dots, such as 192.168.1.20")
		}
	}
	return nil
}

// Summary is the setting in a line, for the audit log.
func (s Setting) Summary() string {
	words := "DHCP"
	if s.Mode == "static" {
		words = "static " + s.Address
		if s.Gateway != "" {
			words += ", gateway " + s.Gateway
		}
	}
	if len(s.DNS) > 0 {
		words += ", DNS " + strings.Join(s.DNS, " ")
	}
	return words
}
