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
