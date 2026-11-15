package policy

import "testing"

func TestNormalize(t *testing.T) {
	for _, x := range []struct{ in, want string }{{" *.Selectel.RU. ", "*.selectel.ru"}, {"127.0.0.1", "127.0.0.1"}, {"::1", "::1"}} {
		got, e := Normalize(x.in)
		if e != nil || got != x.want {
			t.Errorf("%q: %q %v", x.in, got, e)
		}
	}
	for _, s := range []string{"*", "https://example.com", "a..com", "*.127.0.0.1", "foo:443", "foo/bar", "-bad.com", "foo.*.com", "a\n.com"} {
		if _, e := Normalize(s); e == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
func TestWildcardBoundary(t *testing.T) {
	rules := []string{"*.selectel.ru", "exact.test"}
	for _, x := range []struct {
		host string
		want bool
	}{{"api.selectel.ru", true}, {"deep.api.selectel.ru", true}, {"API.SELECTEL.RU.", true}, {"selectel.ru", false}, {"notselectel.ru", false}, {"api.selectel.ru.evil", false}, {"exact.test", true}, {"sub.exact.test", false}} {
		if Match(rules, x.host) != x.want {
			t.Errorf("wrong match for %q", x.host)
		}
	}
}
