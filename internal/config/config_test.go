package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.ListenAddr != ":2222" {
		t.Errorf("ListenAddr = %q, want :2222", c.ListenAddr)
	}
	if c.PrefixKey != "Ctrl-D" {
		t.Errorf("PrefixKey = %q, want Ctrl-D", c.PrefixKey)
	}
	if c.ScrollbackLines != 10000 {
		t.Errorf("ScrollbackLines = %d, want 10000", c.ScrollbackLines)
	}
	if c.ResizePolicy != SmallestWins {
		t.Errorf("ResizePolicy = %d, want SmallestWins", c.ResizePolicy)
	}
	if c.DialTimeout != 10*time.Second {
		t.Errorf("DialTimeout = %s, want 10s", c.DialTimeout)
	}
	if c.DataDir == "" || c.HostKeyPath == "" {
		t.Errorf("DataDir/HostKeyPath must be non-empty: %q %q", c.DataDir, c.HostKeyPath)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DataDir != dir {
		t.Errorf("DataDir = %q, want %q", c.DataDir, dir)
	}
	want := filepath.Join(dir, defaultKeyName)
	if c.HostKeyPath != want {
		t.Errorf("HostKeyPath = %q, want %q", c.HostKeyPath, want)
	}
	if c.ListenAddr != ":2222" {
		t.Errorf("ListenAddr = %q, want default :2222", c.ListenAddr)
	}
}

func TestLoadOverlayMerges(t *testing.T) {
	dir := t.TempDir()
	listen := ":9000"
	scroll := 500
	policy := "driver"
	timeout := "30s"
	fc := fileConfig{
		ListenAddr:      &listen,
		ScrollbackLines: &scroll,
		ResizePolicy:    &policy,
		DialTimeout:     &timeout,
	}
	raw, _ := json.Marshal(fc)
	if err := os.WriteFile(filepath.Join(dir, configFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %q, want :9000", c.ListenAddr)
	}
	if c.ScrollbackLines != 500 {
		t.Errorf("ScrollbackLines = %d, want 500", c.ScrollbackLines)
	}
	if c.ResizePolicy != Driver {
		t.Errorf("ResizePolicy = %d, want Driver", c.ResizePolicy)
	}
	if c.DialTimeout != 30*time.Second {
		t.Errorf("DialTimeout = %s, want 30s", c.DialTimeout)
	}
	// Unset fields keep defaults.
	if c.PrefixKey != "Ctrl-D" {
		t.Errorf("PrefixKey = %q, want default", c.PrefixKey)
	}
}

func TestLoadInvalidScrollback(t *testing.T) {
	dir := t.TempDir()
	scroll := 0
	fc := fileConfig{ScrollbackLines: &scroll}
	raw, _ := json.Marshal(fc)
	os.WriteFile(filepath.Join(dir, configFileName), raw, 0o600)
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for scrollback_lines = 0")
	}
}

func TestEnsureHostKeyGeneratesOnce(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.DataDir = dir
	c.HostKeyPath = filepath.Join(dir, defaultKeyName)

	s1, err := EnsureHostKey(c)
	if err != nil {
		t.Fatalf("EnsureHostKey (generate): %v", err)
	}
	if _, err := os.Stat(c.HostKeyPath); err != nil {
		t.Errorf("private key not written: %v", err)
	}
	if _, err := os.Stat(c.HostKeyPath + ".pub"); err != nil {
		t.Errorf("public key not written: %v", err)
	}

	s2, err := EnsureHostKey(c)
	if err != nil {
		t.Fatalf("EnsureHostKey (reload): %v", err)
	}
	if string(s1.PublicKey().Marshal()) != string(s2.PublicKey().Marshal()) {
		t.Error("reloaded key differs from generated key")
	}
}

func TestParsePrefix(t *testing.T) {
	cases := map[string]byte{
		"Ctrl-Space": 0x00,
		"ctrl-space": 0x00,
		"Ctrl-B":     0x02,
		"C-b":        0x02,
		"ctrl-a":     0x01,
		"^z":         0x1a,
	}
	for in, want := range cases {
		got, err := ParsePrefix(in)
		if err != nil {
			t.Errorf("ParsePrefix(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePrefix(%q) = %#x, want %#x", in, got, want)
		}
	}
	if _, err := ParsePrefix("F1"); err == nil {
		t.Error("expected error for unsupported prefix F1")
	}
}
