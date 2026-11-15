package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitBootMatchesCurrentBoot(t *testing.T) {
	for _, marker := range []string{"", "previous-boot", "current-boot"} {
		t.Run("marker="+marker, func(t *testing.T) {
			dir := t.TempDir()
			params := filepath.Join(dir, "param.env")
			done := filepath.Join(dir, "boot-done")
			if err := os.WriteFile(params, []byte("LIMA_CIDATA_IID='current-boot'\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if marker != "" {
				if err := os.WriteFile(done, []byte(marker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.NewReplacer("/mnt/lima-cidata/lima.env", quote(params), "/run/lima-boot-done", quote(done), "sudo -n sed", "sed").Replace(waitBootScript)
			// Finish provisioning during the first sleep; no real wait or sudo.
			prefix := "sleep() { echo current-boot > " + quote(done) + "; };\n"
			out, err := exec.Command("bash", "-c", prefix+script).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if waited := strings.Contains(string(out), "Waiting for VM provisioning"); waited != (marker != "current-boot") {
				t.Fatalf("wrong readiness decision: %s", out)
			}
		})
	}
}

func TestWaitBootMissingMetadataTimesOut(t *testing.T) {
	dir := t.TempDir()
	script := strings.NewReplacer("/mnt/lima-cidata/lima.env", quote(filepath.Join(dir, "absent")), "/run/lima-boot-done", quote(filepath.Join(dir, "absent-too")), "sudo -n sed", "sed", "attempt<120", "attempt<2").Replace(waitBootScript)
	out, err := exec.Command("bash", "-c", "sleep() { :; };\n"+script).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "VM provisioning did not finish") {
		t.Fatalf("missing metadata accepted: %v %s", err, out)
	}
}

func TestStartDoesNotInstallDuringIncompleteBoot(t *testing.T) {
	a := testApp(t)
	bin := t.TempDir()
	for name, body := range map[string]string{
		"limactl": "printf '{\"status\":\"Running\"}'\n",
		"ssh":     "cat > " + quote(filepath.Join(bin, "script")) + "; exit 42\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ZBOX_GUEST_BINARY", filepath.Join(bin, "unused-helper"))
	if err := a.start(); err == nil || !strings.Contains(err.Error(), "VM boot is not ready") {
		t.Fatalf("unexpected start result: %v", err)
	}
	script, err := os.ReadFile(filepath.Join(bin, "script"))
	if err != nil || string(script) != waitBootScript {
		t.Fatalf("ran installation before boot completed: %v %q", err, script)
	}
}

func TestRestartFlagRequiresCommand(t *testing.T) {
	if err := Main([]string{"--restart"}); err == nil || !strings.Contains(err.Error(), "zbox --restart claude .") {
		t.Fatalf("missing actionable restart help: %v", err)
	}
}

func TestHarnessWithoutArgumentsUsesCurrentDirectory(t *testing.T) {
	a := testApp(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "limactl"), []byte("#!/bin/sh\nif [ \"$1\" = list ]; then printf '{\"status\":\"Running\"}'; else exit 99; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"claude", "codex", "pi", "ori", "openrouter", "mount"} {
		// A valid current directory reaches the automatic restart. The fake
		// stop fails, so no session or daemon is started by this test.
		err := Main([]string{"--state-dir", a.Root, "--vm", a.Name, name})
		if err == nil || !strings.Contains(err.Error(), "exit status 99") {
			t.Fatalf("%s did not reach the mount restart: %v", name, err)
		}
	}
}

func TestStatusBeforeCreate(t *testing.T) {
	a, err := New(t.TempDir(), "new", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.status(); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedConfigMigratesBootProvisioning(t *testing.T) {
	a := testApp(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "args")
	if err := os.WriteFile(filepath.Join(bin, "limactl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+quote(log)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c, _ := a.load()
	c.Mounts = []Mount{{Host: "/host/project", Guest: "/home/agent/dev/project", ReadOnly: true}}
	if err := a.syncVMConfig(c); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := a.template(c)
	var v struct{ Mounts, Provision json.RawMessage }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	want := ".mounts = " + string(v.Mounts) + " | .provision = " + string(v.Provision)
	if !strings.Contains(string(data), want) {
		t.Fatalf("existing VM did not get current mounts and boot scripts: %s", data)
	}
	for _, escaped := range []string{`\u003e`, `\u003c`, `\u0026`} {
		if strings.Contains(string(data), escaped) {
			t.Fatalf("yq would corrupt shell metacharacters: found %s", escaped)
		}
	}
}

func TestBootLogStreamsAndStopsOnCancellation(t *testing.T) {
	a := testApp(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\necho '[zbox] Installing Node.js...'; exec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { a.followBootLog(ctx, &out); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("boot log follower outlived startup")
	}
	if !strings.Contains(out.String(), "Installing Node.js") {
		t.Fatal("boot progress was hidden")
	}
}
