package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFinalizeMP4CopiesAtomicallyAndRemovesStaging(t *testing.T) {
	root := t.TempDir()
	staging, media := filepath.Join(root, "staging"), filepath.Join(root, "media")
	id := selectionID
	source := filepath.Join(staging, id, id)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, []byte("payload")...)
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := finalizeMP4(staging, media, id, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(media, id+".mp4") {
		t.Fatalf("path=%s", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("staging remains: %v", err)
	}
	if _, err := finalizeMP4(staging, media, id, int64(len(payload))); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestFinalizeMP4RejectsSymlinkWrongSizeAndNonMP4(t *testing.T) {
	for _, name := range []string{"symlink", "size", "type"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			staging, media := filepath.Join(root, "s"), filepath.Join(root, "m")
			source := filepath.Join(staging, selectionID, selectionID)
			if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
				t.Fatal(err)
			}
			payload := []byte{0, 0, 0, 12, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
			want := int64(len(payload))
			if name == "symlink" {
				target := filepath.Join(root, "target")
				_ = os.WriteFile(target, payload, 0o600)
				_ = os.Symlink(target, source)
			} else {
				_ = os.WriteFile(source, payload, 0o600)
			}
			if name == "size" {
				want++
			}
			if name == "type" {
				_ = os.WriteFile(source, []byte("not an mp4!!"), 0o600)
			}
			if _, err := finalizeMP4(staging, media, selectionID, want); err == nil {
				t.Fatal("accepted unsafe staging")
			}
		})
	}
}
