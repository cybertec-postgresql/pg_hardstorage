// anomaly.go — AnomalyDetector: refuses LLM-proposed commands that look off-topic for the conversation.
package safety

import (
	"fmt"
	"sort"
	"strings"
)

// AnomalyDetector is the "anomaly refusal" layer of the
// safety stack.  It runs *after* the four hard gates (read-
// only / skill-policy / mutation-flag / preview-replay) and
// flags commands that look out of place given the
// conversation's context — even when the model technically
// has permission to run them.
//
// Examples it catches:
//
//   - The chat is about RESTORE, the model proposes
//     `pg_hardstorage kms shred`.  Wildly off-topic;
//     refuse.
//   - The conversation references deployment `db1`, the
//     model proposes `pg_hardstorage backup db2`.  Wrong
//     deployment; refuse with a confused-context note.
//   - The model proposes a command containing a verb the
//     skill never asked for in its `available_tools`
//     (defence in depth — the skill-policy gate above
//     already catches this, but the anomaly layer logs the
//     attempt for the audit chain).
//
// Anomaly refusal is OPT-IN strict.  Operators who want
// every off-topic command refused outright run with
// --anomaly-refusal=strict; the default is `warn` (allow
// but emit a warning event).  An anomaly score of `severe`
// always refuses regardless of the configured threshold.
type AnomalyDetector struct {
	// HighRiskVerbs are the command-line verbs that REQUIRE a
	// matching topic mention in the recent conversation.
	// Default: ["shred","wipe","delete","force","purge",
	// "rotate","kms","gc"].  Operators add custom verbs via
	// the config file.
	HighRiskVerbs []string

	// DeploymentScope, when non-empty, scopes every command
	// to a specific deployment name.  A high-risk verb that
	// targets a different deployment fires a `severe` anomaly.
	DeploymentScope string

	// RecentTopicTokens is the lowercased set of keywords
	// extracted from the conversation so far.  Verbs in
	// HighRiskVerbs must appear here for the command to be
	// `normal`; otherwise the anomaly layer escalates.
	RecentTopicTokens map[string]struct{}
}

// AnomalyScore is the detector's verdict.
type AnomalyScore int

const (
	// ScoreNormal: command fits the conversation context.
	ScoreNormal AnomalyScore = iota
	// ScoreWarn: command is plausible but worth surfacing
	// as a warning event.
	ScoreWarn
	// ScoreSevere: command is wildly inconsistent with the
	// conversation; refuse outright.
	ScoreSevere
)

// String returns the stable lowercase label ("normal", "warn",
// "severe") for an AnomalyScore. Unknown values format as
// "unknown-N" for diagnostic output.
func (s AnomalyScore) String() string {
	switch s {
	case ScoreNormal:
		return "normal"
	case ScoreWarn:
		return "warn"
	case ScoreSevere:
		return "severe"
	}
	return fmt.Sprintf("unknown-%d", int(s))
}

// AnomalyDecision is the detector's response.
type AnomalyDecision struct {
	Score  AnomalyScore
	Reason string
	Verb   string // the matched high-risk verb, if any
	Token  string // the conversation topic tokens that supported the verdict
}

// DefaultHighRiskVerbs is the verb list every detector
// uses unless the operator overrides.  The list intentionally
// includes the verbs that, if mis-fired, cause irreversible
// data loss or take a deployment offline.
var DefaultHighRiskVerbs = []string{
	"shred", "wipe", "delete", "force", "purge", "rotate", "gc",
}

// Score runs the anomaly checks against cmd and returns the
// strongest verdict.
func (d *AnomalyDetector) Score(cmd string) AnomalyDecision {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return AnomalyDecision{Score: ScoreNormal}
	}

	verbs := d.HighRiskVerbs
	if verbs == nil {
		verbs = DefaultHighRiskVerbs
	}
	tokens := tokensOf(cmd)

	// Scope check: a deployment-scoped detector refuses
	// commands that name a different deployment.
	if d.DeploymentScope != "" {
		if other := mentionsDifferentDeployment(tokens, d.DeploymentScope); other != "" {
			return AnomalyDecision{
				Score:  ScoreSevere,
				Reason: fmt.Sprintf("command targets deployment %q but the conversation is scoped to %q", other, d.DeploymentScope),
				Token:  other,
			}
		}
	}

	for _, verb := range verbs {
		if !commandUsesVerb(tokens, verb) {
			continue
		}
		// High-risk verb fired.  Did the conversation
		// recently discuss it?
		if d.RecentTopicTokens != nil {
			if _, ok := d.RecentTopicTokens[strings.ToLower(verb)]; ok {
				return AnomalyDecision{
					Score:  ScoreNormal,
					Reason: "high-risk verb is on-topic for this conversation",
					Verb:   verb,
					Token:  verb,
				}
			}
		}
		// Off-topic high-risk verb: severe.
		return AnomalyDecision{
			Score:  ScoreSevere,
			Reason: fmt.Sprintf("command contains high-risk verb %q but the conversation didn't reference it", verb),
			Verb:   verb,
		}
	}
	return AnomalyDecision{Score: ScoreNormal}
}

// tokensOf returns the lowercased command tokens.  Splits on
// whitespace and `=` (so `--reason=GDPR` becomes both `--reason`
// and `GDPR`).
func tokensOf(cmd string) []string {
	cmd = strings.ToLower(cmd)
	cmd = strings.ReplaceAll(cmd, "=", " ")
	return strings.Fields(cmd)
}

// commandUsesVerb checks whether any token equals or contains
// the verb.  We match the verb at word boundaries to avoid
// false positives ("delete" doesn't fire on "deleterious").
func commandUsesVerb(tokens []string, verb string) bool {
	v := strings.ToLower(verb)
	for _, t := range tokens {
		if t == v {
			return true
		}
		// also match `--verb` and `--verb-something`
		if strings.HasPrefix(t, "--"+v) {
			return true
		}
	}
	return false
}

// mentionsDifferentDeployment scans tokens for a deployment
// name that isn't the scope.  Returns the offending name or
// empty.  Heuristic: tokens that look like deployment names
// are everything that's not a flag, not a verb keyword, and
// matches a basic identifier shape.
func mentionsDifferentDeployment(tokens []string, scope string) string {
	scope = strings.ToLower(scope)
	keywords := map[string]bool{
		"pg_hardstorage": true, "backup": true, "restore": true, "list": true,
		"show": true, "status": true, "doctor": true, "repo": true, "verify": true,
		"audit": true, "wal": true, "kms": true, "deployment": true, "db": true,
	}
	for _, t := range tokens {
		if strings.HasPrefix(t, "-") {
			continue
		}
		if keywords[t] {
			continue
		}
		// Looks like a deployment name?  Letters + digits +
		// underscore + hyphen, length 2..40.
		if !looksLikeIdentifier(t) {
			continue
		}
		if t != scope {
			return t
		}
	}
	return ""
}

func looksLikeIdentifier(s string) bool {
	if len(s) < 2 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// ExtractTopicTokens is a helper for the chat orchestrator to
// build the RecentTopicTokens set from the assistant's recent
// turns.  It returns lowercased single-word tokens of length
// >= 3 that aren't common stop-words; callers union these
// across the conversation.
func ExtractTopicTokens(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range tokensOf(text) {
		if len(t) < 3 {
			continue
		}
		if isStopWord(t) {
			continue
		}
		out[t] = struct{}{}
	}
	return out
}

var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true,
	"that": true, "this": true, "which": true, "what": true, "who": true,
	"how": true, "why": true, "when": true, "where": true, "are": true,
	"was": true, "were": true, "have": true, "has": true, "had": true,
	"will": true, "would": true, "should": true, "can": true, "may": true,
	"not": true, "you": true, "your": true, "ours": true, "theirs": true,
}

func isStopWord(s string) bool { return stopWords[s] }

// MergeTopicTokens unions multiple token sets.  Used by the
// chat orchestrator to keep a running tally over the last N
// assistant turns.
func MergeTopicTokens(sets ...map[string]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range sets {
		for k := range s {
			out[k] = struct{}{}
		}
	}
	return out
}

// SortedTokens returns the keys of a token set, sorted.
// Useful for audit-event bodies where deterministic ordering
// matters.
func SortedTokens(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
