package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestDocumentedConfigExamplesLoad feeds every config-shaped YAML block
// in docs/ through the production decoder (KnownFields: true) and
// validate().
//
// Operators paste these. v1.4 found all three `compat translate`
// commands emitting YAML the loader rejected — keys that exist nowhere
// in DeploymentConfig — and nothing had ever asked the loader. The same
// can happen to a doc example the moment a field is renamed.
//
// Two shapes are checked: blocks whose top-level keys are all Config
// keys (loaded as-is), and blocks whose top-level keys are all
// DeploymentConfig keys (a `retention:` / `schedule:` fragment, wrapped
// into one deployment). Anything else is prose-adjacent YAML (Helm
// values, Kubernetes manifests) and is left alone.
func TestDocumentedConfigExamplesLoad(t *testing.T) {
	keys := func(v any) map[string]bool {
		m := map[string]bool{}
		ty := reflect.TypeOf(v)
		for i := 0; i < ty.NumField(); i++ {
			if k := strings.Split(ty.Field(i).Tag.Get("yaml"), ",")[0]; k != "" && k != "-" {
				m[k] = true
			}
		}
		return m
	}
	cfgKeys, depKeys := keys(Config{}), keys(DeploymentConfig{})
	top := regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+):`)
	fence := regexp.MustCompile("(?s)```ya?ml\n(.*?)```")

	checked := 0
	err := filepath.WalkDir("../../docs", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(raw)
		for _, m := range fence.FindAllStringSubmatchIndex(src, -1) {
			body := src[m[2]:m[3]]
			tops := top.FindAllStringSubmatch(body, -1)
			if len(tops) == 0 {
				continue
			}
			allCfg, allDep := true, true
			for _, x := range tops {
				allCfg = allCfg && cfgKeys[x[1]]
				allDep = allDep && depKeys[x[1]]
			}
			doc := body
			switch {
			case allCfg:
			case allDep:
				var b strings.Builder
				b.WriteString("deployments:\n  example:\n")
				for _, l := range strings.Split(body, "\n") {
					b.WriteString("    " + l + "\n")
				}
				doc = b.String()
			default:
				continue
			}
			checked++
			line := strings.Count(src[:m[2]], "\n") + 1
			c, _, lerr := loadBytes([]byte(doc), path, "doc")
			if lerr == nil {
				lerr = validate(c)
			}
			if lerr != nil {
				t.Errorf("%s:%d: documented config does not load:\n    %v", path, line, lerr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 40 {
		t.Fatalf("only %d config examples found — extraction is broken, not the docs", checked)
	}
	t.Logf("loaded %d documented config examples", checked)
}
