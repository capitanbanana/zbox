// Package policy implements the host-owned split-routing list.
package policy

import (
	"fmt"
	"net"
	"strings"
)

func Normalize(s string) (string, error) {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	base := strings.TrimPrefix(s, "*.")
	if net.ParseIP(base) != nil {
		if base != s {
			return "", fmt.Errorf("wildcards require a DNS name")
		}
		return s, nil
	}
	if len(base) == 0 || len(base) > 253 {
		return "", fmt.Errorf("invalid hostname %q", s)
	}
	for _, label := range strings.Split(base, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid hostname %q", s)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("use a hostname, IP, or *.domain (ASCII/punycode): %q", s)
			}
		}
	}
	return s, nil
}

// *.example.com matches subdomains, never the apex or notexample.com.
func Match(rules []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, rule := range rules {
		if strings.HasPrefix(rule, "*.") {
			if strings.HasSuffix(host, rule[1:]) && len(host) > len(rule)-1 {
				return true
			}
		} else if host == rule {
			return true
		}
	}
	return false
}
