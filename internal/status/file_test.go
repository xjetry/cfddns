package status

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type atomicTestPayload struct {
	Version int    `json:"version"`
	Message string `json:"message"`
}

func TestAtomicWriteJSONModeAndContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfddns.status")
	want := atomicTestPayload{Version: 1, Message: "ready"}

	if err := writeJSONAtomic(path, want); err != nil {
		t.Fatalf("writeJSONAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("invalid JSON after rename: %s", data)
	}
	var got atomicTestPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("payload = %+v, want %+v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o644 {
		t.Fatalf("mode = %04o, want 0644", gotMode)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "cfddns.status" {
		t.Fatalf("directory contains leftover temporary files: %+v", entries)
	}
}

func TestAtomicWriteJSONReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfddns.status")
	if err := os.WriteFile(path, []byte(`{"version":0,"message":"old"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want := atomicTestPayload{Version: 2, Message: "new"}

	if err := writeJSONAtomic(path, want); err != nil {
		t.Fatalf("writeJSONAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got atomicTestPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal replacement %s: %v", data, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("replacement = %+v, want %+v", got, want)
	}
}

func TestAtomicWriteJSONMissingParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "missing")
	path := filepath.Join(parent, "cfddns.status")

	if err := writeJSONAtomic(path, atomicTestPayload{Version: 1}); err == nil {
		t.Fatal("writeJSONAtomic succeeded with a missing parent directory")
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writer created the missing parent or returned an unexpected Stat error: %v", err)
	}
}

func TestAtomicWriteJSONRenameErrorRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfddns.status")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir destination: %v", err)
	}

	if err := writeJSONAtomic(path, atomicTestPayload{Version: 1}); err == nil {
		t.Fatal("writeJSONAtomic succeeded when rename destination was a directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "cfddns.status" || !entries[0].IsDir() {
		t.Fatalf("rename failure left temporary files or changed destination: %+v", entries)
	}
}
