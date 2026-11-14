// Package system holds what the console and its root helper both check about the System page's settings:
// the unit's name, the clock and the power mode (docs/console.md).
package system

import (
	"errors"
	"regexp"
	"strconv"
)

// The name the owner keeps for this unit, read at boot by patai-identity.
const NameFile = "/persist/provision/hostname"

var name = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// CheckName says what's wrong with a name, in words for the owner. It's one DNS label, because it's also
// <name>.local, and it starts with a letter so that it can't be taken for an address.
func CheckName(n string) error {
	switch {
	case n == "":
		return errors.New("give the unit a name")
	case len(n) > 63:
		return errors.New("a name can be 63 characters at most")
	case n == "localhost":
		return errors.New("localhost is taken")
	case !name.MatchString(n):
		return errors.New("use lower-case letters, digits and hyphens, starting with a letter and not ending in a hyphen")
	}
	return nil
}

// ValidTime is a clock setting's shape, milliseconds since 1970: patai-clock decides whether it can be right.
func ValidTime(ms int64) bool { return ms > 0 && ms < 1e15 }

// FormatTime is the argument patai-clock takes.
func FormatTime(ms int64) string { return strconv.FormatInt(ms, 10) }

var powerMode = regexp.MustCompile(`^[0-9]{1,2}$`)

// ValidPowerMode is an nvpmodel id's shape: patai-power-mode checks this release has it.
func ValidPowerMode(id string) bool { return powerMode.MatchString(id) }
