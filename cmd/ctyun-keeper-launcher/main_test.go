//go:build linux

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vay1314/CtYun-Keeper/internal/update"
)

func TestChooseStartupVersionKeepsNewerOnlineVersion(t *testing.T) {
	root := t.TempDir()
	builtin := filepath.Join(root, "builtin")
	online := filepath.Join(root, "versions", "v2.2.0")
	link := filepath.Join(root, "runtime", "current")
	writeTestVersion(t, builtin, "2.1.0")
	writeTestVersion(t, online, "2.2.0")
	if err := switchCurrent(link, online); err != nil {
		t.Fatal(err)
	}
	launcherVersion = "1.0.0"
	fallback, err := chooseStartupVersion(link, builtin)
	if err != nil {
		t.Fatal(err)
	}
	if fallback != "" || resolveCurrent(link, "") != online {
		t.Fatalf("newer online version was replaced: fallback=%q current=%q", fallback, resolveCurrent(link, ""))
	}
}

func TestDevelopmentImageKeepsInstalledFormalVersion(t *testing.T) {
	root := t.TempDir()
	builtin := filepath.Join(root, "builtin")
	online := filepath.Join(root, "versions", "v2.2.0")
	link := filepath.Join(root, "runtime", "current")
	writeTestVersion(t, builtin, "dev-abc123")
	writeTestVersion(t, online, "2.2.0")
	if err := switchCurrent(link, online); err != nil {
		t.Fatal(err)
	}
	launcherVersion = "1.0.0"
	fallback, err := chooseStartupVersion(link, builtin)
	if err != nil {
		t.Fatal(err)
	}
	if fallback != "" || resolveCurrent(link, "") != online {
		t.Fatalf("development image replaced installed formal version: fallback=%q current=%q", fallback, resolveCurrent(link, ""))
	}
}

func TestTrustedUpdatePublicKeyUsesEnvironmentOnlyAsFallback(t *testing.T) {
	original := updatePublicKey
	t.Cleanup(func() { updatePublicKey = original })
	t.Setenv("CTYUN_UPDATE_PUBLIC_KEY", "environment-key")
	updatePublicKey = ""
	if got := trustedUpdatePublicKey(); got != "environment-key" {
		t.Fatalf("development key fallback = %q", got)
	}
	updatePublicKey = "embedded-key"
	if got := trustedUpdatePublicKey(); got != "embedded-key" {
		t.Fatalf("environment replaced embedded release key: %q", got)
	}
}

func TestInstallPolicyEnforcesSignedImageRequirement(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	digest := strings.Repeat("a", 64)
	manifest := update.Manifest{
		SchemaVersion: 1, Version: "2.2.0", Tag: "v2.2.0", RequiresImageUpdate: true,
		Assets: map[string]update.Asset{"linux-" + runtime.GOARCH: {Name: "release.tar.gz", Size: 1, SHA256: digest, PackageManifestSHA256: digest}},
	}
	raw, _ := json.Marshal(manifest)
	req := update.InstallRequest{
		Action: "install", FromVersion: "2.1.0", ToVersion: "2.2.0",
		PackageManifestSHA256: digest, SignedManifest: raw,
		ManifestSignature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, raw)),
	}
	originalKey, originalLauncher := updatePublicKey, launcherVersion
	t.Cleanup(func() { updatePublicKey, launcherVersion = originalKey, originalLauncher })
	updatePublicKey = base64.StdEncoding.EncodeToString(publicKey)
	launcherVersion = "1.0.0"
	builtinDir := t.TempDir()
	writeTestVersion(t, builtinDir, "2.1.0")
	t.Setenv("CTYUN_IMAGE_VERSION", "2.2.0")
	if err := verifyInstallPolicy(req, builtinDir); err == nil {
		t.Fatal("signed requiresImageUpdate policy was ignored")
	}
	writeTestVersion(t, builtinDir, "2.2.0")
	t.Setenv("CTYUN_IMAGE_VERSION", "2.1.0")
	if err := verifyInstallPolicy(req, builtinDir); err != nil {
		t.Fatalf("matching updated image was rejected: %v", err)
	}
}

func TestFirstBootUsesBuiltinAndRejectsIncompatibleOnline(t *testing.T) {
	root := t.TempDir()
	builtin := filepath.Join(root, "builtin")
	online := filepath.Join(root, "online")
	link := filepath.Join(root, "runtime", "current")
	launcherVersion = "1.0.0"
	writeTestVersion(t, builtin, "2.0.0")
	if _, err := chooseStartupVersion(link, builtin); err != nil {
		t.Fatal(err)
	}
	if resolveCurrent(link, "") != builtin {
		t.Fatal("first boot did not use builtin")
	}
	writeTestVersion(t, online, "2.1.0")
	if err := writeVersion(online, "2.1.0", "9.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := switchCurrent(link, online); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CTYUN_IMAGE_UPDATE_REQUIRED", "")
	if _, err := chooseStartupVersion(link, builtin); err != nil {
		t.Fatal(err)
	}
	if resolveCurrent(link, "") != builtin || os.Getenv("CTYUN_IMAGE_UPDATE_REQUIRED") == "" {
		t.Fatal("incompatible online version not rejected with a notice")
	}
}

func TestChooseStartupVersionPromotesNewerBuiltin(t *testing.T) {
	root := t.TempDir()
	builtin := filepath.Join(root, "builtin")
	online := filepath.Join(root, "versions", "v2.1.0")
	link := filepath.Join(root, "runtime", "current")
	writeTestVersion(t, builtin, "2.2.0")
	writeTestVersion(t, online, "2.1.0")
	if err := switchCurrent(link, online); err != nil {
		t.Fatal(err)
	}
	launcherVersion = "1.0.0"
	fallback, err := chooseStartupVersion(link, builtin)
	if err != nil {
		t.Fatal(err)
	}
	if fallback != online || resolveCurrent(link, "") != builtin {
		t.Fatalf("builtin was not promoted: fallback=%q current=%q", fallback, resolveCurrent(link, ""))
	}
}

func writeTestVersion(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(versionInfo{Version: version, MinimumLauncherVersion: "1.0.0"})
	if err := os.WriteFile(filepath.Join(dir, "version.json"), raw, 0640); err != nil {
		t.Fatal(err)
	}
}

func TestConsumeRequestRejectsMissingSidecarToken(t *testing.T) {
	dataDir := t.TempDir()
	current := filepath.Join(dataDir, "runtime", "versions", "v2.1.0")
	if err := os.MkdirAll(filepath.Join(dataDir, "updates"), 0750); err != nil {
		t.Fatal(err)
	}
	req := update.InstallRequest{
		Action: "install", FromVersion: "2.1.0", ToVersion: "2.2.0",
		PackageManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		StagingDir:            filepath.Join(dataDir, "updates", "staging", "2.2.0"),
		Executable:            filepath.Join(current, "ctyun-keeper"),
		BackupDir:             filepath.Join(dataDir, "updates", "backups", "2.1.0"),
		DatabasePath:          filepath.Join(dataDir, "ctyun-keeper.db"),
		DatabaseBackup:        filepath.Join(dataDir, "updates", "backups", "2.1.0", "database", "ctyun-keeper.db"),
		HealthURL:             "http://127.0.0.1:9845/health", ParentPID: 1,
		RestartMode: "self", Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), HistoryID: 1,
		ResultPath: filepath.Join(dataDir, "updates", "results", "1.json"),
	}
	if err := update.WriteInstallRequest(filepath.Join(dataDir, "updates", "install-request.json"), req); err != nil {
		t.Fatal(err)
	}
	if _, err := consumeRequest(dataDir, current); err == nil {
		t.Fatal("request without sidecar token was accepted")
	}
}

func TestLauncherLockRejectsSecondInstance(t *testing.T) {
	dataDir := t.TempDir()
	first, err := acquireLauncherLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLauncherLock(first)
	if second, err := acquireLauncherLock(dataDir); err == nil {
		releaseLauncherLock(second)
		t.Fatal("second launcher acquired the same data volume")
	}
}

func TestPreparePendingRestoresDatabaseAndSwitchesVersion(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "current")
	target := filepath.Join(root, "target")
	previous := filepath.Join(root, "previous")
	for _, dir := range []string{target, previous} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
	}
	if err := switchCurrent(current, previous); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "database.db")
	backup := filepath.Join(root, "backup.db")
	if err := os.WriteFile(database, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	pending := &pendingUpdate{Request: update.InstallRequest{DatabasePath: database}, Target: target, TargetDatabase: backup}
	if err := preparePending(pending, current); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(database)
	if err != nil || string(contents) != "old" || resolveCurrent(current, "") != target {
		t.Fatalf("pending transaction was not replayed: database=%q current=%q err=%v", contents, resolveCurrent(current, ""), err)
	}
}

func TestValidatePendingRestrictsVersionPaths(t *testing.T) {
	dataDir := t.TempDir()
	builtinDir := filepath.Join(dataDir, "builtin")
	versionsDir := filepath.Join(dataDir, "runtime", "versions")
	previous := filepath.Join(versionsDir, "v2.1.0")
	writeTestVersion(t, builtinDir, "2.2.0")
	writeTestVersion(t, previous, "2.1.0")
	pending := &pendingUpdate{
		Request: update.InstallRequest{
			Action: "image", FromVersion: "2.1.0", ToVersion: "2.2.0",
			Token: strings.Repeat("a", 64), HistoryID: 9,
			DatabasePath: filepath.Join(dataDir, "ctyun-keeper.db"),
			ResultPath:   filepath.Join(dataDir, "updates", "results", "9.json"),
		},
		Previous: previous, Target: builtinDir, SuccessStatus: "success",
	}
	if err := validatePending(pending, dataDir, builtinDir, versionsDir); err != nil {
		t.Fatalf("valid pending update was rejected: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	writeTestVersion(t, outside, "2.2.0")
	pending.Target = outside
	if err := validatePending(pending, dataDir, builtinDir, versionsDir); err == nil {
		t.Fatal("pending update outside trusted version roots was accepted")
	}
}

func TestCompletePendingKeepsJournalWhenResultWriteFails(t *testing.T) {
	dataDir := t.TempDir()
	pending := &pendingUpdate{
		Request: update.InstallRequest{
			FromVersion: "2.1.0", ToVersion: "2.2.0", Token: strings.Repeat("b", 64), HistoryID: 10,
			ResultPath: filepath.Join(dataDir, "updates", "results", "10.json"),
		},
		SuccessStatus: "success", SuccessMessage: "updated",
	}
	if err := savePending(dataDir, pending); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "updates", "results"), []byte("blocks directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := completePending(dataDir, pending); err == nil {
		t.Fatal("result write failure was ignored")
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "updates", "pending.json"))
	if err != nil {
		t.Fatalf("pending journal was removed: %v", err)
	}
	var saved pendingUpdate
	if err := json.Unmarshal(raw, &saved); err != nil || saved.CompletedResult == nil {
		t.Fatalf("durable completion marker missing: %v", err)
	}
}
