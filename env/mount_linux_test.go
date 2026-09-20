package env

import (
	"path/filepath"
	"testing"

	"github.com/criyle/go-sandbox/pkg/mount"
)

// assertJava17ReadonlyMount verifies JDK 17's configuration directory is
// bind-mounted read-only and that the whole /etc directory is not exposed.
func assertJava17ReadonlyMount(t *testing.T, b *mount.Builder) {
	t.Helper()
	const wantTarget = "etc/java-17-openjdk"
	var found bool
	for _, m := range b.Mounts {
		if m.Target == "etc" || m.Target == "/etc" {
			t.Fatalf("whole /etc must not be mounted: %v", m)
		}
		if m.Source != "/etc/java-17-openjdk" {
			continue
		}
		found = true
		if m.Target != wantTarget {
			t.Errorf("java17 mount target = %q, want %q", m.Target, wantTarget)
		}
		if !m.IsBindMount() {
			t.Errorf("java17 mount is not a bind mount: %v", m)
		}
		if !m.IsReadOnly() {
			t.Errorf("java17 mount must be read-only: %v", m)
		}
	}
	if !found {
		t.Error("java17 configuration directory is not mounted")
	}
}

func TestGetDefaultMountJava17(t *testing.T) {
	assertJava17ReadonlyMount(t, getDefaultMount("size=128m,nr_inodes=4k"))
}

func TestParseMountConfigJava17(t *testing.T) {
	m, err := readMountConfig(filepath.Join("..", "mount.yaml"))
	if err != nil {
		t.Fatalf("read mount.yaml: %v", err)
	}
	b, err := parseMountConfig(m)
	if err != nil {
		t.Fatalf("parse mount.yaml: %v", err)
	}
	assertJava17ReadonlyMount(t, b)
}
