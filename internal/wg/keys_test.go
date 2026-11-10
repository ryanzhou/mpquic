package wg

import "testing"

func TestKeys(t *testing.T) {
	// RFC 7748 section 6.1 test vector.
	priv, err := ParseKey("dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCo=")
	if err != nil {
		t.Fatal(err)
	}
	if got := priv.PublicKey().String(); got != "hSDwCYkwp1R0i33ctD73Wg2/Og0mOBr066SpjqqbTmo=" {
		t.Fatalf("public key %s", got)
	}
	for _, bad := range []string{"", "abc", "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LC==", "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCoA"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) succeeded", bad)
		}
	}
	k, _ := GenerateKey()
	if k2, err := ParseKey(k.String()); err != nil || k2 != k || k[0]&7 != 0 || k[31]&0xc0 != 0x40 {
		t.Fatal("generated key does not round-trip or is not clamped")
	}
}

func TestCheck(t *testing.T) {
	k, _ := GenerateKey()
	pk := k.PublicKey().String()
	ok := Options{Listen: ":51820", PrivateKey: k.String(), Peers: []Peer{{PublicKey: pk}}}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"no key":         {Listen: ":1", Peers: ok.Peers},
		"no peers":       {Listen: ":1", PrivateKey: k.String()},
		"bad listen":     {Listen: "example.com:1", PrivateKey: k.String(), Peers: ok.Peers},
		"bad port":       {Listen: ":70000", PrivateKey: k.String(), Peers: ok.Peers},
		"bad allowed":    {Listen: ":1", PrivateKey: k.String(), Peers: []Peer{{PublicKey: pk, AllowedIPs: []string{"10.0.0.300"}}}},
		"two peers, any": {Listen: ":1", PrivateKey: k.String(), Peers: []Peer{{PublicKey: pk, AllowedIPs: []string{"10.0.0.2"}}, {PublicKey: pk}}},
		"mtu":            {Listen: ":1", PrivateKey: k.String(), Peers: ok.Peers, MTU: 500},
	} {
		if err := o.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
