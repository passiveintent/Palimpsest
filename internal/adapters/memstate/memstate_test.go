package memstate

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStore_SaveLoad_InMemory(t *testing.T) {
	s := New("")
	ctx := context.Background()

	if _, ok, err := s.Load(ctx, "missing"); err != nil || ok {
		t.Fatalf("Load(missing) = (ok=%v err=%v), want (false, nil)", ok, err)
	}

	if err := s.Save(ctx, "k", []byte("v1")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, ok, err := s.Load(ctx, "k")
	if err != nil || !ok || string(data) != "v1" {
		t.Fatalf("Load(k) = (%q, %v, %v), want (v1, true, nil)", data, ok, err)
	}
}

func TestStore_PersistsAndReloadsFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.gob")
	ctx := context.Background()

	s1 := New(path)
	if err := s1.Save(ctx, "a", []byte("1")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s1.Save(ctx, "b", []byte("2")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2 := New(path)
	if err := s2.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	for k, want := range map[string]string{"a": "1", "b": "2"} {
		got, ok, err := s2.Load(ctx, k)
		if err != nil || !ok || string(got) != want {
			t.Fatalf("Load(%s) = (%q, %v, %v), want (%s, true, nil)", k, got, ok, err, want)
		}
	}
}

func TestStore_LoadFromDisk_MissingFileIsNotError(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "does-not-exist.gob"))
	if err := s.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk (missing file): %v, want nil", err)
	}
}
