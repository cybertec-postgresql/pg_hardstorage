package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	rendererjson "github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/renderer/json"
)

// A sink's config is a free-form map, so a misspelt key was ignored
// without a word (docs/operations/operator-guide.md shipped an email
// sink with tls_mode/auth_mode/password_secret for months; the plugin
// reads tls/auth/password). These tests keep the declared key lists,
// the plugins' sources and the documentation in step.

func repoRootForSinkTest(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(here), "..", ".."))
}

// sinkPackageDirs maps each registered plugin name to its package dir.
func sinkPackageDirs(t *testing.T, root string) map[string]string {
	t.Helper()
	re := regexp.MustCompile(`DefaultSinkRegistry\.Register\("([a-z0-9-]+)"`)
	out := map[string]string{}
	dirs, _ := filepath.Glob(filepath.Join(root, "internal", "plugin", "sink", "*"))
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			for _, m := range re.FindAllSubmatch(b, -1) {
				out[string(m[1])] = d
			}
		}
	}
	return out
}

// TestSinkPlugins_DeclareEveryKeyTheyRead: every key a plugin's source
// reads from its top-level config must be declared, or start-up would
// warn about a key the plugin does honour.
func TestSinkPlugins_DeclareEveryKeyTheyRead(t *testing.T) {
	root := repoRootForSinkTest(t)
	readKey := regexp.MustCompile(`(?:SinkConfigString(?:Default)?\((?:spec\.Config|cfg), |spec\.Config\[|readStringList\(spec\.Config, )"([a-z0-9_]+)"`)
	dirs := sinkPackageDirs(t, root)
	if len(dirs) < 10 {
		t.Fatalf("found only %d sink plugins; the scan is broken", len(dirs))
	}
	for plugin, dir := range dirs {
		declared, ok := output.DefaultSinkRegistry.DeclaredConfigKeys(plugin)
		if !ok {
			t.Errorf("sink plugin %q declares no config keys (DeclareConfigKeys)", plugin)
			continue
		}
		set := map[string]bool{}
		for _, k := range declared {
			set[k] = true
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, _ := os.ReadFile(f)
			for _, m := range readKey.FindAllSubmatch(b, -1) {
				if k := string(m[1]); !set[k] {
					t.Errorf("sink plugin %q reads config key %q (%s) but does not declare it", plugin, k, filepath.Base(f))
				}
			}
		}
	}
}

// TestDocumentedSinkConfigsUseDeclaredKeys: every sink configured in a
// documentation YAML example may only use keys its plugin reads.
func TestDocumentedSinkConfigsUseDeclaredKeys(t *testing.T) {
	root := repoRootForSinkTest(t)
	fence := regexp.MustCompile("(?s)```ya?ml\n(.*?)```")
	checked := 0
	var bad []string
	_ = filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		body, _ := os.ReadFile(path)
		for _, m := range fence.FindAllSubmatch(body, -1) {
			var doc any
			if yaml.Unmarshal(m[1], &doc) != nil {
				continue
			}
			walkSinkSpecs(doc, func(plugin string, cfg map[string]any) {
				unknown := output.DefaultSinkRegistry.UnknownConfigKeys(output.SinkSpec{Plugin: plugin, Config: cfg})
				if _, declared := output.DefaultSinkRegistry.DeclaredConfigKeys(plugin); declared {
					checked++
				}
				for _, k := range unknown {
					rel, _ := filepath.Rel(root, path)
					bad = append(bad, rel+": plugin "+plugin+": unknown key "+k)
				}
			})
		}
		return nil
	})
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
	if checked == 0 {
		t.Fatal("found no documented sink configs; the scan is broken")
	}
}

// walkSinkSpecs calls fn for every {plugin: <name>, config: {...}} map
// anywhere in a decoded YAML document.
func walkSinkSpecs(v any, fn func(string, map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		if p, ok := x["plugin"].(string); ok {
			if cfg, ok := x["config"].(map[string]any); ok {
				fn(p, cfg)
			}
		}
		for _, c := range x {
			walkSinkSpecs(c, fn)
		}
	case []any:
		for _, c := range x {
			walkSinkSpecs(c, fn)
		}
	}
}

// TestAttachLoadedSinks_WarnsOnUnknownConfigKey: start-up names a key
// the plugin ignores, and still attaches the sink.
func TestAttachLoadedSinks_WarnsOnUnknownConfigKey(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := output.NewDispatcher(rendererjson.New(), &stdout, &stderr)
	attachLoadedSinks(context.Background(), d, &config.LoadResult{Config: config.Config{
		Sinks: []output.SinkSpec{{Name: "ops-email", Plugin: "email", Config: map[string]any{
			"smtp_host": "127.0.0.1", "from": "a@example.com", "to": []any{"b@example.com"},
			"auth": "none", "tls": "none", "tls_mode": "starttls",
		}}},
	}})
	all := stdout.String() + stderr.String()
	if !strings.Contains(all, "sink.unknown_config_keys") || !strings.Contains(all, "tls_mode") {
		t.Fatalf("no sink.unknown_config_keys warning naming tls_mode:\n%s", all)
	}
	if strings.Contains(all, "sink.build_failed") {
		t.Fatalf("an unknown key must warn, not disable the sink:\n%s", all)
	}
}
