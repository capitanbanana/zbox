package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHarnessArgumentsAndOriMCP(t *testing.T) {
	args := []string{"--model", "provider/model", "a b", "$(false)", "'", ""}
	for _, name := range []string{"pi", "ori", "openrouter"} {
		want := append([]string{name}, args...)
		if name == "openrouter" {
			want[0] = "ori"
		}
		if got := harnessCommand(name, args, Config{}); !reflect.DeepEqual(got, want) {
			t.Fatal(name, got, want)
		}
	}
	got := harnessCommand("ori", []string{"claude", "--model", "provider/model", "prompt"}, Config{})
	want := append([]string{"ori", "claude"}, mcpArgs("claude", Config{})...)
	want = append(want, "--model", "provider/model", "prompt")
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got, want)
	}
	for _, sub := range []string{"login", "auth", "codex", "pi"} {
		args := []string{sub, "--help"}
		if got := harnessCommand("ori", args, Config{}); !reflect.DeepEqual(got, append([]string{"ori"}, args...)) {
			t.Fatal(got)
		}
	}
	a := testApp(t)
	if !strings.Contains(a.sessionCommand("ori", "/mnt/project", nil, Config{}), "export CODEX_HOME='/home/agent/.config/agents/ori-codex'") {
		t.Fatal("Ori shares ordinary Codex auth")
	}
	if strings.Contains(a.sessionCommand("codex", "/mnt/project", nil, Config{}), "ori-codex") {
		t.Fatal("plain Codex uses Ori state")
	}
}

func TestHarnessProvisionMigration(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Model an old VM with Claude/Codex and existing auth; installers are stubs
	// so this checks migration, failure handling and idempotence without network.
	write("claude", "exit 0\n")
	write("codex", "exit 0\n")
	auth := filepath.Join(dir, "existing-auth")
	if err := os.WriteFile(auth, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	write("node", "exit 0\n")
	write("npm", `if [ "$1" = config ]; then exit 0; fi
echo "$*" >> "$AGENTS_TEST_HOME/install.log"
if [ "${AGENTS_FAIL_NPM:-}" = 1 ]; then exit 42; fi
test "$*" = 'install -g --ignore-scripts @earendil-works/pi-coding-agent'
printf '#!/bin/sh\nexit 0\n' > "$AGENTS_TEST_HOME/.local/bin/pi"
chmod +x "$AGENTS_TEST_HOME/.local/bin/pi"
`)
	write("curl", `echo curl "$@" >> "$AGENTS_TEST_HOME/install.log"
url=""
dest=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; dest="$1" ;;
    http*) url="$1" ;;
  esac
  shift
done
case "$url" in
  *astral.sh*)
    cat > "$dest" <<'UV'
test "$UV_INSTALL_DIR" = "$AGENTS_TEST_HOME/.local/bin"
cat > "$UV_INSTALL_DIR/uv" <<'UVBIN'
#!/bin/sh
tool="$AGENTS_TEST_HOME/.local/share/uv/tools/claude-code-redact/bin"
mkdir -p "$tool"
printf '#!/bin/sh\nexit 0\n' > "$tool/rdx"
printf '#!/bin/sh\nexit 0\n' > "$tool/python"
chmod +x "$tool/rdx" "$tool/python"
site="$AGENTS_TEST_HOME/.local/share/uv/tools/claude-code-redact/lib/python3.12/site-packages/rdx/proxy"
mkdir -p "$site"
printf 'print(_cache.stats())\n' > "$site/server.py"
UVBIN
chmod +x "$UV_INSTALL_DIR/uv"
UV
    ;;
  *)
    cat > "$dest" <<'INSTALL'
test "$ORI_INSTALL_DIR" = "$AGENTS_TEST_HOME/.local/bin"
printf '#!/bin/sh\nexit 0\n' > "$ORI_INSTALL_DIR/ori"
chmod +x "$ORI_INSTALL_DIR/ori"
INSTALL
    ;;
esac
`)
	script := strings.ReplaceAll(harnessProvision, "$HOME", dir)
	runScript := func(fail bool) error {
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), "AGENTS_TEST_HOME="+dir)
		if fail {
			cmd.Env = append(cmd.Env, "AGENTS_FAIL_NPM=1")
		}
		_, err := cmd.CombinedOutput()
		return err
	}
	if err := runScript(true); err == nil {
		t.Fatal("failed install reported success")
	}
	if _, err := os.Stat(filepath.Join(bin, "ori")); !os.IsNotExist(err) {
		t.Fatal("continued after install failure")
	}
	if err := runScript(false); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "install.log"))
	if err := runScript(false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "install.log"))
	if string(before) != string(after) {
		t.Fatal("reinstalled existing tools")
	}
	if b, _ := os.ReadFile(auth); string(b) != "keep" {
		t.Fatal("lost auth")
	}
	for _, name := range []string{"claude", "codex", "pi", "ori"} {
		if st, e := os.Stat(filepath.Join(bin, name)); e != nil || st.Mode()&0111 == 0 {
			t.Fatal("tool missing", name, e)
		}
	}
	patched := filepath.Join(dir, ".local", "share", "uv", "tools", "claude-code-redact",
		"lib", "python3.12", "site-packages", "rdx", "proxy", "server.py")
	if data, e := os.ReadFile(patched); e != nil || strings.Contains(string(data), "_cache.") {
		t.Fatalf("rdx proxy patch not applied: %v %q", e, data)
	}
	for _, name := range []string{"rdx", "python"} {
		path := filepath.Join(dir, ".local", "share", "uv", "tools", "claude-code-redact", "bin", name)
		if st, e := os.Stat(path); e != nil || st.Mode()&0111 == 0 {
			t.Fatal("rdx tool missing", name, e)
		}
	}
}

func TestTemplateDoesNotInstallCLIsDuringBoot(t *testing.T) {
	a := testApp(t)
	c, _ := a.load()
	b, err := a.template(c)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Provision []struct{ Mode, Script string }
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Provision) != 1 || v.Provision[0].Mode != "system" {
		t.Fatal("boot should only install system dependencies")
	}
	for _, p := range v.Provision {
		if strings.Contains(p.Script, "npm install") || strings.Contains(p.Script, "uv tool install") {
			t.Fatal("CLI downloads block VM boot")
		}
	}
}
