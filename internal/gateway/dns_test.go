package gateway

import "github.com/miekg/dns"

func dnsQuestion() *dns.Msg { return new(dns.Msg).SetQuestion("example.com.", dns.TypeA) }
