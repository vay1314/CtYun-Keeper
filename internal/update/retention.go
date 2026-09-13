package update

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func CleanupArtifacts(dataDir string) {
	pruneRecoveryDatabases(filepath.Join(dataDir, "updates", "recovery"), 1, 7*24*time.Hour)
	pruneDirectories(filepath.Join(dataDir, "updates", "staging"), 0, 24*time.Hour)
	pruneFiles(filepath.Join(dataDir, "updates", "helper"), 24*time.Hour)
	pruneFiles(filepath.Join(dataDir, "updates", "downloads"), 24*time.Hour)
}

// Run only after 24 hours of healthy operation. Always preserve the current,
// previous successful and Docker image versions, regardless of directory timestamps.
func CleanupRetainedVersions(dataDir, current, previous, imageVersion string) {
	protected := map[string]bool{}
	for _, version := range []string{current, previous, imageVersion} {
		if version != "" {
			protected[version] = true
			protected["v"+version] = true
		}
	}
	pruneRetained(filepath.Join(dataDir, "runtime", "versions"), protected, 3)
	pruneRetained(filepath.Join(dataDir, "updates", "backups"), protected, 3)
}

func pruneRetained(root string, protected map[string]bool, keep int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type candidate struct {
		name string
		mod  time.Time
	}
	var candidates []candidate
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		count++
		if protected[entry.Name()] {
			continue
		}
		if info, err := entry.Info(); err == nil {
			candidates = append(candidates, candidate{entry.Name(), info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mod.Before(candidates[j].mod) })
	for _, item := range candidates {
		if count <= keep {
			break
		}
		if err := os.RemoveAll(filepath.Join(root, item.name)); err == nil {
			count--
		}
	}
}

func pruneDirectories(root string, keep int, olderThan time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type item struct {
		path string
		mod  time.Time
	}
	var items []item
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			items = append(items, item{filepath.Join(root, entry.Name()), info.ModTime()})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	for index, value := range items {
		if (keep > 0 && index >= keep) || (olderThan > 0 && time.Since(value.mod) > olderThan) {
			_ = os.RemoveAll(value.path)
		}
	}
}

func pruneFiles(root string, olderThan time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err == nil && time.Since(info.ModTime()) > olderThan {
			_ = os.Remove(filepath.Join(root, entry.Name()))
		}
	}
}

func pruneRecoveryDatabases(root string, keep int, olderThan time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type item struct {
		base string
		mod  time.Time
	}
	groups := map[string]time.Time{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "image-") {
			continue
		}
		base := entry.Name()
		switch {
		case strings.HasSuffix(base, ".db-wal"):
			base = strings.TrimSuffix(base, "-wal")
		case strings.HasSuffix(base, ".db-shm"):
			base = strings.TrimSuffix(base, "-shm")
		case !strings.HasSuffix(base, ".db"):
			continue
		}
		info, infoErr := entry.Info()
		if infoErr == nil && info.ModTime().After(groups[base]) {
			groups[base] = info.ModTime()
		}
	}
	items := make([]item, 0, len(groups))
	for base, mod := range groups {
		items = append(items, item{base: base, mod: mod})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	for index, value := range items {
		if (keep > 0 && index >= keep) || (olderThan > 0 && time.Since(value.mod) > olderThan) {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(filepath.Join(root, value.base+suffix))
			}
		}
	}
}
