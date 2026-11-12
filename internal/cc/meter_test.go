package cc

import "testing"

func TestProbeGateSerialises(t *testing.T) {
	var g ProbeGate
	a, b := &Meter{}, &Meter{}
	a.SetProbeGate(&g)
	b.SetProbeGate(&g)
	if !a.gate(true) {
		t.Fatal("first path denied PROBE_RTT")
	}
	if !a.gate(true) {
		t.Fatal("holder denied re-entry")
	}
	if b.gate(true) {
		t.Fatal("second path may probe while the first one is probing")
	}
	a.gate(false)
	if !b.gate(true) {
		t.Fatal("second path denied after the first one finished")
	}
	g.Release(b)
	if !a.gate(true) {
		t.Fatal("gate not released")
	}
	if !(&Meter{}).gate(true) {
		t.Fatal("meter without a gate must always be allowed")
	}
}

func TestParseProfile(t *testing.T) {
	// the variants of the earlier BBRv1 controller still parse, as BBR
	for _, in := range []string{"", "bbr", "BBR", "bbr-aggressive", "bbr-conservative"} {
		if p, err := ParseProfile(in); err != nil || p != ProfileBBR {
			t.Errorf("ParseProfile(%q) = %q, %v", in, p, err)
		}
	}
	if p, err := ParseProfile("cubic"); err != nil || p != ProfileCubic {
		t.Errorf("ParseProfile(cubic) = %q, %v", p, err)
	}
	if _, err := ParseProfile("vegas"); err == nil {
		t.Error("vegas accepted")
	}
}
