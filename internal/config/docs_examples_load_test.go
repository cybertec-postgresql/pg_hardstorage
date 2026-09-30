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

// TestDocumentedConfigClaimsAreReal closes the gap the test above leaves
// open. That one only loads blocks whose keys it already recognises, so a
// block claiming a config section that does not exist was skipped as
// "not config" — exactly how four did ship:
//
//	observability:  (monitoring.md — tracing)
//	server:         (scaling-large-fleets.md, and `server --help`)
//	plugins:        (tier2 plugin protocol — RPC timeout)
//	approvals:      (llm-safety-stack.md — n-of-m thresholds)
//
// None exists. Because the file is decoded with KnownFields, an operator
// who pasted any of them did not get an ignored setting: every command
// refused to load the configuration.
//
// Here a YAML block counts as a claim about pg_hardstorage.yaml when the
// prose just before it says so, and it is not recognisably another
// tool's YAML (Kubernetes, Helm values, compose, Patroni, skill files,
// testkit files). Every top-level key of such a block must exist.
func TestDocumentedConfigClaimsAreReal(t *testing.T) {
	cfgKeys := map[string]bool{}
	ty := reflect.TypeOf(Config{})
	for i := 0; i < ty.NumField(); i++ {
		if k := strings.Split(ty.Field(i).Tag.Get("yaml"), ",")[0]; k != "" && k != "-" {
			cfgKeys[k] = true
		}
	}
	claim := regexp.MustCompile(`(?i)pg_hardstorage\.yaml|config(uration)? file|in (the )?config\b|via config|configurable (per|via)`)
	foreign := regexp.MustCompile(`(?m)^(apiVersion|kind|services|jobs|on|steps|scrape_configs|groups|route|receivers|image|replicaCount|config|env|persistence|serviceAccount|keyring|permanent_slots|bootstrap|postgresql|schema|trigger|permissions|context|guardrails|locales|fleet|profiles|faults|scenario|topology):`)
	top := regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+):`)
	fence := regexp.MustCompile("(?s)```ya?ml\n(.*?)```")

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
			pre := src[max(0, m[0]-400):m[0]]
			if foreign.MatchString(body) || !claim.MatchString(pre) {
				continue
			}
			for _, k := range top.FindAllStringSubmatch(body, -1) {
				if !cfgKeys[k[1]] {
					t.Errorf("%s:%d: documented as pg_hardstorage.yaml but %q is not a config section — "+
						"pasting it makes every command fail to load the configuration",
						path, strings.Count(src[:m[2]], "\n")+1, k[1])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
