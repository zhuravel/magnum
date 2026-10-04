package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInteractiveRejectsNonTerminals(t *testing.T) {
	if Interactive(nil) {
		t.Fatal("nil is not a terminal")
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if st, err := null.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("%s should be a character device (the case the old check got wrong): %v", os.DevNull, err)
	}
	if Interactive(null) {
		t.Fatalf("%s is not a terminal", os.DevNull)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if Interactive(r) || Interactive(w) {
		t.Fatal("a pipe is not a terminal")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if Interactive(f) {
		t.Fatal("a regular file is not a terminal")
	}
}
