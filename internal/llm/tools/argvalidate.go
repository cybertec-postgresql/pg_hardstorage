// argvalidate.go — validation of model-supplied tool arguments before they reach a child argv.
package tools

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
)

// Every string a live-state tool puts into the child's argv comes from
// the model, and the model's input is attacker-influenced (prompt
// injection through logs, doc excerpts, audit entries).  The child
// parses root persistent flags from anywhere in argv, so a value like
// "--cpu-profile=/home/op/.bashrc" is not a harmless wrong answer: it
// truncates a file.  Two independent layers keep that out:
//
//  1. these validators refuse anything that could parse as a flag (a
//     leading '-') or that does not fit the value's grammar; and
//  2. the tools place positionals after a `--` terminator, so even a
//     value that slipped past (1) is read as a positional.
//
// The validators are deliberately strict — a refused tool call only
// costs the model a retry, an accepted smuggled flag costs a file.

// argString extracts an optional string argument.  A present but
// non-string value is an error rather than silently ignored, so the
// model learns its call was malformed.
func argString(tool string, args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: %s must be a string, got %T", tool, key, v)
	}
	return s, nil
}

// checkNotFlag refuses a leading dash and control characters — the
// shapes that can change how the child parses its argv.  Identifiers
// (repo, backup id) additionally refuse whitespace; free-text filters
// ("2 days ago") may carry it.
func checkNotFlag(tool, key, v string, allowSpace bool) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("%s: %s %q must not start with '-' (it would be parsed as a flag)", tool, key, v)
	}
	for _, r := range v {
		if unicode.IsControl(r) || (!allowSpace && unicode.IsSpace(r)) {
			return fmt.Errorf("%s: %s %q contains whitespace or a control character", tool, key, v)
		}
	}
	return nil
}

// validDeploymentArg enforces the config's deployment-name grammar
// ([a-zA-Z][a-zA-Z0-9_-]{0,62}), which also rules out a leading '-'.
func validDeploymentArg(tool, v string) error {
	if err := config.ValidDeploymentName(v); err != nil {
		return fmt.Errorf("%s: invalid deployment: %w", tool, err)
	}
	return nil
}

// validRepoArg accepts a repository URL with a scheme or a bare
// absolute path — the forms storage.Open takes.
func validRepoArg(tool, v string) error {
	if err := checkNotFlag(tool, "repo", v, false); err != nil {
		return err
	}
	if strings.HasPrefix(v, "/") {
		return nil
	}
	if u, err := url.Parse(v); err != nil || u.Scheme == "" {
		return fmt.Errorf("%s: repo %q must be a URL (s3://, file:///, ...) or an absolute path", tool, v)
	}
	return nil
}

// validBackupIDArg accepts "latest" or a storage-safe backup id
// ("<dep>.<type>.<ts>.<hex>"): no path separators, no leading '-'.
func validBackupIDArg(tool, v string) error {
	if err := checkNotFlag(tool, "backup_id", v, false); err != nil {
		return err
	}
	if v == "." || v == ".." || strings.ContainsAny(v, `/\`) {
		return fmt.Errorf("%s: backup_id %q is not a valid backup id", tool, v)
	}
	return nil
}

// validFilterArg is for free-form filter values (audit action, since):
// anything that cannot be mistaken for a flag.
func validFilterArg(tool, key, v string) error {
	return checkNotFlag(tool, key, v, true)
}
