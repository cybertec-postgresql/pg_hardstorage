// homebrew_cask_test.go — the cask reaches the tap through a PR.
//
// The homebrew-tap repository's `main` requires a reviewed pull request.
// goreleaser pushed the cask straight to `main`, the tap rejected it with
// 409, and the v1.5.0 release run failed after every asset had uploaded,
// leaving Homebrew on 1.4.2. The cask must go through a branch and a PR.
package regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHomebrewCaskIsPublishedThroughAPullRequest(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Casks []struct {
			Name       string `yaml:"name"`
			Repository struct {
				Name        string `yaml:"name"`
				Branch      string `yaml:"branch"`
				PullRequest struct {
					Enabled bool `yaml:"enabled"`
					Base    struct {
						Name   string `yaml:"name"`
						Branch string `yaml:"branch"`
					} `yaml:"base"`
				} `yaml:"pull_request"`
			} `yaml:"repository"`
		} `yaml:"homebrew_casks"`
	}
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Casks) == 0 {
		t.Fatal("no homebrew_casks entry in .goreleaser.yaml")
	}
	for _, c := range cfg.Casks {
		r := c.Repository
		if !r.PullRequest.Enabled {
			t.Errorf("cask %q: repository.pull_request.enabled must be true — the tap's main rejects direct pushes", c.Name)
		}
		if r.Branch == "" || r.Branch == "main" || !strings.Contains(r.Branch, "{{") {
			t.Errorf("cask %q: repository.branch must be a per-release branch, got %q", c.Name, r.Branch)
		}
		if r.PullRequest.Base.Name != r.Name || r.PullRequest.Base.Branch != "main" {
			t.Errorf("cask %q: the PR must target %s:main, got %s:%s", c.Name, r.Name,
				r.PullRequest.Base.Name, r.PullRequest.Base.Branch)
		}
	}
}
