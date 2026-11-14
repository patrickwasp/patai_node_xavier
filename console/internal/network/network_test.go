package network

import "testing"

func TestCheck(t *testing.T) {
	good := []Setting{
		{Mode: "dhcp"},
		{Mode: "dhcp", DNS: []string{"1.1.1.1"}},
		{Mode: "static", Address: "192.168.1.20/24", Gateway: "192.168.1.1", DNS: []string{"192.168.1.1", "1.1.1.1"}},
	}
	for _, s := range good {
		if err := s.Check(); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := []Setting{
		{Mode: ""},
		{Mode: "bridge"},
		{Mode: "static", Address: "192.168.1.20/24\nDNS=6.6.6.6"},
		{Mode: "static", Address: "192.168.1.20/24", Gateway: "x"},
		{Mode: "dhcp", DNS: []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4"}},
		{Mode: "dhcp", DNS: []string{"dns.example"}},
		{Mode: "static", Address: "1111111111111111111/24"},
	}
	for _, s := range bad {
		if err := s.Check(); err == nil {
			t.Errorf("%+v passed", s)
		}
	}
}

func TestNames(t *testing.T) {
	for port, ok := range map[string]bool{"gbe1": true, "gbe2": true, "eth0": false, "gbe1 ": false, "../gbe1": false, "": false} {
		if ValidPort(port) != ok {
			t.Errorf("%q: %v", port, !ok)
		}
	}
	for id, ok := range map[string]bool{"0123456789abcdef": true, "0123456789ABCDEF": false, "0123": false, "--keep": false} {
		if ValidID(id) != ok {
			t.Errorf("%q: %v", id, !ok)
		}
	}
	if Label("gbe2") != "GbE 2" {
		t.Error(Label("gbe2"))
	}
}

func TestSummary(t *testing.T) {
	cases := map[string]Setting{
		"DHCP":                      {Mode: "dhcp"},
		"DHCP, DNS 1.1.1.1 1.0.0.1": {Mode: "dhcp", DNS: []string{"1.1.1.1", "1.0.0.1"}},
		"static 10.0.0.5/24":        {Mode: "static", Address: "10.0.0.5/24"},
		"static 10.0.0.5/24, gateway 10.0.0.1, DNS 10.0.0.1": {Mode: "static", Address: "10.0.0.5/24",
			Gateway: "10.0.0.1", DNS: []string{"10.0.0.1"}},
	}
	for want, s := range cases {
		if got := s.Summary(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
