package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentPlatformReadsImageVersionFromBuiltinFile(t *testing.T) {
	builtinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(builtinDir, "version.json"), []byte(`{"version":"2.0.5","minimumLauncherVersion":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CTYUN_CONTAINER", "true")
	t.Setenv("BUILTIN_DIR", builtinDir)
	t.Setenv("CTYUN_IMAGE_VERSION", "2.0.4")
	t.Setenv("CTYUN_BUILTIN_VERSION", "2.0.4")

	platform := CurrentPlatform()
	if platform.ImageVersion != "2.0.5" {
		t.Fatalf("stale environment replaced image metadata: image=%q", platform.ImageVersion)
	}
}

func TestReadVersionFileRejectsMissingVersion(t *testing.T) {
	builtinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(builtinDir, "version.json"), []byte(`{"minimumLauncherVersion":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVersionFile(builtinDir); err == nil {
		t.Fatal("version file without a version was accepted")
	}
}
