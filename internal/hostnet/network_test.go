package hostnet

import "testing"

func homeNetwork() Network {
	return Network{Identity: Identity{Subnet: "10.77.0.0/24", Gateway: "10.77.0.1", MAC: "02:00:00:00:00:01"}, Interface: "en0", Index: 6, Source: "10.77.0.10"}
}

func TestRouteAndARPParsing(t *testing.T) {
	g, i, err := parseRoute("route to: default\n gateway: 10.77.0.1\n interface: en0\n")
	if err != nil || g != "10.77.0.1" || i != "en0" {
		t.Fatal(g, i, err)
	}
	for _, raw := range []string{"", "gateway: link#6\ninterface: en0", "gateway: 127.0.0.1\ninterface: lo0", "gateway: 10.77.0.1\ninterface: en0\ninterface: en1"} {
		if _, _, err := parseRoute(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	m, err := parseARP("? (10.77.0.1) at 2:a:0:0:0:1 on en0 ifscope [ethernet]\n", g, i)
	if err != nil || m != "02:0a:00:00:00:01" {
		t.Fatal(m, err)
	}
	for _, raw := range []string{
		"? (10.77.0.1) at (incomplete) on en0 ifscope [ethernet]",
		"? (10.77.0.2) at 2:a:0:0:0:1 on en0 ifscope [ethernet]",
		"? (10.77.0.1) at 2:a:0:0:0:1 on en1 ifscope [ethernet]",
		"? (10.77.0.1) at 2:a:0:0:0:1 on en0 ifscope [ethernet]\nother entry",
	} {
		if _, err := parseARP(raw, g, i); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff", "01:00:00:00:00:01", "garbage"} {
		if _, err := normalizeMAC(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestMatchRequiresFullIdentity(t *testing.T) {
	home := homeNetwork()
	rules := []Exception{{Name: "home", Identity: home.Identity}}
	if Match(home, rules) != "home" {
		t.Fatal("home did not match")
	}
	ethernet := home
	ethernet.Interface = "en5"
	ethernet.Index = 12
	ethernet.Source = "10.77.0.11"
	if Match(ethernet, rules) != "home" {
		t.Fatal("same LAN on ethernet did not match")
	}
	for _, mutate := range []func(*Network){
		func(n *Network) { n.MAC = "02:00:00:00:00:02" },
		func(n *Network) { n.Gateway = "10.77.0.254" },
		func(n *Network) { n.Subnet = "10.77.0.0/25" },
		func(n *Network) { n.Source = "10.77.1.1" },
		func(n *Network) { n.Index = 0 },
		func(n *Network) { n.MAC = "" },
	} {
		n := home
		mutate(&n)
		if Match(n, rules) != "" {
			t.Fatalf("unexpected trust: %+v", n)
		}
	}
}
