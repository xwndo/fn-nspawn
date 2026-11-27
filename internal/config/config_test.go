package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewStoreRecoversFromCorruptJSON(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"standard_base_root": "/vol1/bad`), 0644); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(tmp)
	if err != nil {
		t.Fatalf("NewStore should self-heal corrupt config, got error: %v", err)
	}
	if store.Get().StandardBaseRoot != filepath.Clean("/vol1/nspawn") {
		t.Fatalf("expected default StandardBaseRoot, got %q", store.Get().StandardBaseRoot)
	}
	if _, err := os.Stat(cfgPath + ".corrupt"); err != nil {
		t.Fatalf("expected .corrupt backup file to be written: %v", err)
	}
}

func TestStoreSaveValidatesPaths(t *testing.T) {
	tmp := t.TempDir()
	store, err := NewStore(tmp)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Save(AppConfig{StandardBaseRoot: "relative/path"}); err == nil {
		t.Fatalf("expected error for relative StandardBaseRoot")
	}
	if err := store.Save(AppConfig{StandardBaseRoot: "/vol1/nspawn\ninjected"}); err == nil {
		t.Fatalf("expected error for newline in StandardBaseRoot")
	}
	if err := store.Save(AppConfig{
		StandardBaseRoot: "/vol1/nspawn",
		BaseRoots:        []string{"relative/scan"},
	}); err == nil {
		t.Fatalf("expected error for relative BaseRoots entry")
	}
	if err := store.Save(AppConfig{
		StandardBaseRoot: tmp,
		BaseRoots:        []string{tmp, "/vol1/nspawn"},
	}); err != nil {
		t.Fatalf("expected valid absolute paths to succeed, got %v", err)
	}
}
