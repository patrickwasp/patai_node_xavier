package system

import (
	"strings"
	"testing"
)

func TestCheckName(t *testing.T) {
	for _, good := range []string{"x", "patai", "lab-2", "a" + strings.Repeat("b", 62)} {
		if err := CheckName(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{"", "localhost", "Xavier", "2nd-unit", "-x", "x-", "x.local", "x_y", "x y",
		"x;reboot", "a" + strings.Repeat("b", 63), "ünit"} {
		if CheckName(bad) == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestShapes(t *testing.T) {
	if !ValidTime(1793636635000) || ValidTime(0) || ValidTime(-1) || ValidTime(1e15) {
		t.Error("time")
	}
	if FormatTime(1793636635000) != "1793636635000" {
		t.Error("format")
	}
	for _, id := range []string{"0", "7", "12"} {
		if !ValidPowerMode(id) {
			t.Errorf("%q", id)
		}
	}
	for _, id := range []string{"", "-1", "123", "0 1", "7;reboot"} {
		if ValidPowerMode(id) {
			t.Errorf("%q passed", id)
		}
	}
}
