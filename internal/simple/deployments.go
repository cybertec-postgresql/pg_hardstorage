// deployments.go — Deployment bundle + deployment list helpers shared by every simple-CLI flow.
package simple

import (
	"errors"
	"fmt"
	"sort"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/simple/prompt"
)

// Deployment is the bundle every flow operates on: name + the two
// settings that drive everything (PG DSN, repo URL).  Pulled from
// the merged config.LoadResult.Config.Deployments map and sorted by
// name for deterministic menu order.
type Deployment struct {
	Name         string
	PGConnection string
	Repo         string
	Tenant       string
}

// deploymentList returns every configured deployment, sorted by
// name.  Empty slice when no config or no deployments — caller
// renders the "no deployments yet, pick #1" hint.
func deploymentList(env *Env) []Deployment {
	if env.Config == nil {
		return nil
	}
	out := make([]Deployment, 0, len(env.Config.Config.Deployments))
	for name, d := range env.Config.Config.Deployments {
		out = append(out, Deployment{
			Name: name, PGConnection: d.PGConnection,
			Repo: d.Repo, Tenant: d.Tenant,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// pickDeployment runs the deployment picker.  Defaults to
// env.State.LastDeployment when set; otherwise the first one in the
// sorted list.  Returns prompt.ErrQuit if the operator types "q".
//
// Returns "no deployments configured" error when the list is empty —
// caller (each flow) prints the menu-#1 hint and unwinds.
func pickDeployment(env *Env, question string) (Deployment, error) {
	deps := deploymentList(env)
	if len(deps) == 0 {
		return Deployment{}, errNoDeployments
	}
	if len(deps) == 1 {
		// One option: don't bother with the menu.  Print what
		// we picked and continue.
		env.Prompter.Printf("  using deployment %q\n", deps[0].Name)
		return deps[0], nil
	}
	defaultIdx := -1
	if env.State != nil {
		for i, d := range deps {
			if d.Name == env.State.LastDeployment {
				defaultIdx = i
				break
			}
		}
	}
	choices := make([]prompt.Choice, len(deps))
	for i, d := range deps {
		choices[i] = prompt.Choice{
			Label:  d.Name,
			Detail: fmt.Sprintf("repo=%s  pg=%s", d.Repo, redactPassword(d.PGConnection)),
		}
	}
	idx, err := env.Prompter.PromptChoice(question, choices, defaultIdx)
	if err != nil {
		return Deployment{}, err
	}
	return deps[idx], nil
}

// errNoDeployments is the sentinel for "operator hasn't run
// menu-item #1 yet".  Flows catch this and print the suggested
// next step instead of a raw error.
var errNoDeployments = errors.New("no deployments configured")

// redactPassword turns `postgres://user:secret@host/db` into
// `postgres://user:***@host/db` for terminal display.  Best-effort —
// we only collapse the standard libpq URL shape; anything weirder
// shows as-is.
func redactPassword(dsn string) string {
	// `postgres://U:P@H/D` → split at "://", then at "@".
	const sep = "://"
	i := indexOf(dsn, sep)
	if i < 0 {
		return dsn
	}
	scheme := dsn[:i+len(sep)]
	rest := dsn[i+len(sep):]
	at := indexOf(rest, "@")
	if at < 0 {
		return dsn // no auth segment
	}
	auth := rest[:at]
	tail := rest[at:]
	colon := indexOf(auth, ":")
	if colon < 0 {
		return dsn
	}
	return scheme + auth[:colon+1] + "***" + tail
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
