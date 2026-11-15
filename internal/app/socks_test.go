package app

import (
	"strings"
	"testing"
)

func TestSOCKSPortValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		port int
	}{
		{nil, 1080}, {[]string{"--port", "1081"}, 1081}, {[]string{"--port=65535"}, 65535},
	} {
		p, err := parseSOCKSPort(tc.args)
		if err != nil || p != tc.port {
			t.Fatal(tc, p, err)
		}
	}
	for _, args := range [][]string{{"--port", "0"}, {"--port", "65536"}, {"--port", "-1"}, {"--port", "localhost:1080"}, {"--listen", "0.0.0.0"}, {"extra"}} {
		if _, err := parseSOCKSPort(args); err == nil {
			t.Fatal("accepted invalid arguments", args)
		}
	}
}

func TestBrowserSOCKSUsesGuestPolicyAndOwnSSHConnection(t *testing.T) {
	a := testApp(t)
	args := a.browserSOCKS(11080).Args
	joined := strings.Join(args, " ")
	for _, required := range []string{"-F " + a.sshConfig(), "-N -T", "ControlMaster=no", "ControlPath=none", "ForwardAgent=no", "ForwardX11=no", "ExitOnForwardFailure=yes", "-L 127.0.0.1:11080:127.0.0.1:18082", "lima-test"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q: %s", required, joined)
		}
	}
	for _, arg := range args {
		if arg == "-D" || arg == "-g" || arg == "-f" || strings.Contains(arg, "0.0.0.0") {
			t.Fatal("unsafe listener or policy bypass", args)
		}
	}
}
