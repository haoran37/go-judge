package testdata

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCasesSortedPairsOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "2.in"), "b")
	writeFile(t, filepath.Join(dir, "2.out"), "B")
	writeFile(t, filepath.Join(dir, "1.in"), "a")
	writeFile(t, filepath.Join(dir, "1.out"), "A")
	writeFile(t, filepath.Join(dir, "3.in"), "ignored")

	cases, err := LoadCases(dir)
	if err != nil {
		t.Fatalf("LoadCases error: %v", err)
	}
	if len(cases) != 2 || cases[0].ID != "1" || cases[1].ID != "2" {
		t.Fatalf("unexpected cases: %+v", cases)
	}
}

func TestUnzipSafeRejectsTraversal(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "bad.zip")
	createZip(t, zipPath, map[string]string{"../1.in": "x"})
	if err := unzipSafe(zipPath, t.TempDir()); err == nil {
		t.Fatal("expected traversal zip entry to be rejected")
	}
}

func TestUnzipSafeExtractsPlainFiles(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "ok.zip")
	createZip(t, zipPath, map[string]string{"1.in": "x", "1.out": "x"})
	dst := t.TempDir()
	if err := unzipSafe(zipPath, dst); err != nil {
		t.Fatalf("unzipSafe error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "1.in")); err != nil {
		t.Fatalf("expected extracted input: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func createZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
