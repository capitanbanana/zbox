package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultStateRootPreservesExistingVMs(t *testing.T) {
	home := t.TempDir()
	current, legacy := filepath.Join(home, ".zbox"), filepath.Join(home, ".agents")
	if got := defaultStateRoot(home); got != current {
		t.Fatalf("fresh install: %s", got)
	}
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if got := defaultStateRoot(home); got != legacy {
		t.Fatalf("existing VMs would be lost: %s", got)
	}
	if err := os.Mkdir(current, 0700); err != nil {
		t.Fatal(err)
	}
	if got := defaultStateRoot(home); got != current {
		t.Fatalf("explicit current state must win: %s", got)
	}
}

func TestSettingEnvPrefersZboxWithLegacyFallback(t *testing.T) {
	for _, name := range []string{"HOME", "GUEST_BINARY", "SING_BOX"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENTS_"+name, "legacy-value")
			t.Setenv("ZBOX_"+name, "")
			if got := settingEnv(name); got != "legacy-value" {
				t.Fatalf("legacy override lost: %s", got)
			}
			t.Setenv("ZBOX_"+name, "current-value")
			if got := settingEnv(name); got != "current-value" {
				t.Fatalf("current override must win: %s", got)
			}
		})
	}
}
