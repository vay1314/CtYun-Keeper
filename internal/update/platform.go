package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Platform struct {
	OS              string
	Arch            string
	InDocker        bool
	LauncherVersion string
	ImageVersion    string
	RuntimeVersion  string
}

func CurrentPlatform() Platform {
	docker := inDocker()
	imageVersion := ""
	if docker {
		builtinDir := strings.TrimSpace(os.Getenv("BUILTIN_DIR"))
		if builtinDir == "" {
			builtinDir = "/app/builtin"
		}
		imageVersion, _ = ReadVersionFile(builtinDir)
	}
	return Platform{
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		InDocker:        docker,
		LauncherVersion: strings.TrimSpace(os.Getenv("CTYUN_LAUNCHER_VERSION")),
		ImageVersion:    imageVersion,
		RuntimeVersion:  strings.TrimSpace(os.Getenv("CTYUN_RUNTIME_VERSION")),
	}
}

func ReadVersionFile(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "version.json"))
	if err != nil {
		return "", err
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("读取 version.json: %w", err)
	}
	info.Version = strings.TrimSpace(info.Version)
	if info.Version == "" {
		return "", fmt.Errorf("version.json 缺少版本")
	}
	if _, ok := ParseSemVer(info.Version); !ok && !IsDevVersion(info.Version) {
		return "", fmt.Errorf("version.json 版本无效")
	}
	return info.Version, nil
}

func (p Platform) AssetKey() string {
	return p.OS + "-" + p.Arch
}

func inDocker() bool {
	if os.Getenv("CTYUN_CONTAINER") == "true" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		return strings.Contains(string(data), "docker")
	}
	return false
}
