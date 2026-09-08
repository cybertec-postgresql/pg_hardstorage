// flags.go — pgBackRest shim flag union + pgbackrest→native arg translator (connection, repo, compression, retention).
package pgbackrest

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/pflag"
)

// pgbackrestArgs is the flat union of every pgBackRest flag the
// shim recognises.  Each verb reads only the subset relevant to
// it; mapToNativeArgs renders the connection / repo flags into
// native CLI args.
//
// Flags are populated by registerCommonFlags on the root's
// persistent flag set; per-verb flags (--type, --target,
// --target-action, ...) attach to the verb's own flag set.
type pgbackrestArgs struct {
	// --stanza=<n>
	stanza string

	// PG endpoint
	pg1Host     string
	pg1Port     int
	pg1Database string
	pg1User     string

	// Repository
	repo1Type       string // posix | s3
	repo1Path       string
	repo1S3Bucket   string
	repo1S3Region   string
	repo1S3Endpoint string
	repo1S3KeyType  string // shared | auto
	repo1CipherType string // none | aes-256-cbc
	repo1CipherPass string

	// Compression / retention / async knobs (not all forwarded)
	compressType  string // zstd | lz4 | gzip | none
	retentionFull int
	archiveAsync  bool

	// Per-verb additions populated where relevant
	backupType   string // full | incr | diff
	target       string // time | LSN | name (recovery target)
	targetAction string // pause | promote | shutdown
	targetType   string // explicit form override (time|lsn|name|immediate)

	// pgBackRest's explicit per-form target flags. These are what the
	// tool's own documentation and most crons use; --target + --type
	// is the older spelling. resolveTarget folds the two together.
	targetTime string
	targetLSN  string
	targetName string
}

// registerCommonFlags wires pgBackRest's persistent flags onto
// the supplied flag set.  Verbs that need extras (e.g. --type
// on `backup`) attach them on their own command's Flags().
func registerCommonFlags(fs *pflag.FlagSet) {
	defaultArgs := &globalArgs

	fs.StringVar(&defaultArgs.stanza, "stanza", "",
		"deployment name (mapped to pg_hardstorage's positional arg)")

	fs.StringVar(&defaultArgs.pg1Host, "pg1-host", "",
		"PostgreSQL host")
	fs.IntVar(&defaultArgs.pg1Port, "pg1-port", 0,
		"PostgreSQL port")
	fs.StringVar(&defaultArgs.pg1Database, "pg1-database", "",
		"PostgreSQL database")
	fs.StringVar(&defaultArgs.pg1User, "pg1-user", "",
		"PostgreSQL user")

	fs.StringVar(&defaultArgs.repo1Type, "repo1-type", "",
		"repository type: posix | s3")
	fs.StringVar(&defaultArgs.repo1Path, "repo1-path", "",
		"posix repository path")
	fs.StringVar(&defaultArgs.repo1S3Bucket, "repo1-s3-bucket", "",
		"S3 bucket")
	fs.StringVar(&defaultArgs.repo1S3Region, "repo1-s3-region", "",
		"S3 region")
	fs.StringVar(&defaultArgs.repo1S3Endpoint, "repo1-s3-endpoint", "",
		"S3 endpoint override (MinIO etc.)")
	fs.StringVar(&defaultArgs.repo1S3KeyType, "repo1-s3-key-type", "",
		"S3 key type (auto | shared) — credentials still come from env")

	// Cipher: pgBackRest defaults to none.  We map cipher-pass
	// to the native KEK-derivation path with a warning that
	// AES-256-GCM is the modern equivalent (vs CBC).
	fs.StringVar(&defaultArgs.repo1CipherType, "repo1-cipher-type", "",
		"repository cipher: none | aes-256-cbc")
	fs.StringVar(&defaultArgs.repo1CipherPass, "repo1-cipher-pass", "",
		"repository cipher passphrase (KEK-derivation source)")

	fs.StringVar(&defaultArgs.compressType, "compress-type", "",
		"compression: zstd | lz4 | gzip | none")
	fs.IntVar(&defaultArgs.retentionFull, "retention-full", 0,
		"keep this many full backups")
	fs.BoolVar(&defaultArgs.archiveAsync, "archive-async", false,
		"async archive_command (refused — native is already async)")

	// Many less-cited pgBackRest knobs are accepted silently
	// and ignored.  Listing them as defined flags lets cobra
	// not bail on parsing.
	for _, ignored := range silentlyIgnoredFlags {
		fs.String(ignored, "", "")
		_ = fs.MarkHidden(ignored)
	}

	// start-fast / stop-auto / backup-standby are BOOLEAN pgBackRest
	// knobs: operators write a bare `--start-fast` (no value), and
	// pgBackRest's own documentation spells the explicit form
	// `--start-fast=y` / `=n`. pflag's native bool only accepts Go's
	// strconv.ParseBool vocabulary, so the documented y/n form failed
	// with a raw "strconv.ParseBool: parsing \"y\"" and no
	// remediation. pgbRestBool accepts both vocabularies.
	for _, ignoredBool := range silentlyIgnoredBoolFlags {
		v := new(pgbRestBool)
		fs.Var(v, ignoredBool, "")
		// NoOptDefVal is what lets a BARE `--start-fast` parse; without
		// it pflag demands a value for a Var flag.
		fs.Lookup(ignoredBool).NoOptDefVal = "y"
		_ = fs.MarkHidden(ignoredBool)
	}

	// Flags pgBackRest accepts that the shim has nothing to do with,
	// but which appear in real cron lines. Accepting them keeps the
	// command line parsing; a bare "unknown flag" from cobra carries
	// no remediation and breaks the migration outright.
	for _, ignored := range silentlyIgnoredValueFlags {
		fs.String(ignored, "", "")
		_ = fs.MarkHidden(ignored)
	}
	for _, ignoredBool := range silentlyIgnoredExtraBoolFlags {
		v := new(pgbRestBool)
		fs.Var(v, ignoredBool, "")
		fs.Lookup(ignoredBool).NoOptDefVal = "y"
		_ = fs.MarkHidden(ignoredBool)
	}

	// Flags pgBackRest accepts and the shim must NOT silently swallow,
	// because ignoring them would change what the operator asked for.
	// Registered so they parse, then refused by name with a pointer at
	// the native equivalent (checkRefusedFlags).
	for name := range refusedFlags {
		if refusedFlagIsBool[name] {
			v := new(pgbRestBool)
			fs.Var(v, name, "")
			fs.Lookup(name).NoOptDefVal = "y"
		} else {
			fs.String(name, "", "")
		}
		_ = fs.MarkHidden(name)
	}
}

// pgbRestBool is a pflag.Value that understands BOTH pgBackRest's
// y/n spelling and Go's true/false, so `--start-fast`,
// `--start-fast=y` and `--start-fast=true` all parse.
type pgbRestBool bool

func (b *pgbRestBool) String() string {
	if b != nil && bool(*b) {
		return "y"
	}
	return "n"
}

func (b *pgbRestBool) Set(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "y", "yes", "true", "1", "on", "":
		*b = true
		return nil
	case "n", "no", "false", "0", "off":
		*b = false
		return nil
	default:
		return fmt.Errorf("invalid boolean %q (pgBackRest spells these y/n)", v)
	}
}

// Type is what pflag prints in usage; "y|n" matches pgBackRest.
func (b *pgbRestBool) Type() string { return "y|n" }

// silentlyIgnoredValueFlags take a value and have no native
// equivalent. --subject is the classic mail-subject cron option;
// --cwd / --log-file are logging/placement concerns the native
// structured output replaces.
var silentlyIgnoredValueFlags = []string{
	"subject",
	"cwd",
	"log-file",
	// Inline S3 credentials. pg_hardstorage takes credentials from the
	// standard SDK chain, so these are not honoured — but they have to
	// PARSE for buildRepoURL's "not honoured" warning to be reachable
	// at all. Previously the flag died at parse time and the warning
	// was dead code.
	"repo1-s3-key",
	"repo1-s3-key-secret",
}

// silentlyIgnoredExtraBoolFlags are booleans pgBackRest accepts whose
// effect the native path already provides or does not need.
var silentlyIgnoredExtraBoolFlags = []string{
	// `verify --online` — native verify reads the repository and does
	// not care whether PG is up.
	"online",
	// `restore --delta` restores only changed files into an existing
	// data directory. Native restore always writes a complete data
	// directory, which is a superset of the delta result: correct, just
	// not incremental. Accepting it keeps the cron working.
	"delta",
}

// refusedFlags maps a pgBackRest flag the shim will not silently
// ignore to the native remediation. Ignoring any of these would
// change the outcome the operator asked for, so they refuse loudly
// (exit 2) instead of parsing into a no-op.
var refusedFlags = map[string]string{
	"force": "native restore refuses a non-empty --target on purpose; " +
		"clear the directory yourself, or restore to a fresh path",
	"dry-run": "use `pg_hardstorage restore ... --preview` for a restore dry-run, " +
		"or `pg_hardstorage rotate ...` without --apply for a retention dry-run",
	"pg1-path": "pg_hardstorage reaches PostgreSQL over the replication protocol, " +
		"not the data directory: set --pg1-host/--pg1-port/--pg1-user instead",
	"repo2-type": "multi-repository stanzas are not translated; run one deployment " +
		"per repository, or replicate with `pg_hardstorage repo replicate`",
	"repo2-path": "multi-repository stanzas are not translated; run one deployment " +
		"per repository, or replicate with `pg_hardstorage repo replicate`",
	"config": "the shim reads pg_hardstorage.yaml, not pgbackrest.conf: convert it " +
		"once with `pg_hardstorage compat translate --from pgbackrest <config-path>`",
}

// refusedFlagIsBool marks which refused flags are pgBackRest booleans,
// so a bare `--force` parses (and is then refused) rather than
// swallowing the next argv element as its value.
var refusedFlagIsBool = map[string]bool{
	"force":   true,
	"dry-run": true,
}

// checkRefusedFlags returns a refusal for the first registered-but-
// refused flag the operator actually set. Verbs call it after parse
// and before dispatching anything.
func checkRefusedFlags(fs *pflag.FlagSet) error {
	var found []string
	fs.Visit(func(f *pflag.Flag) {
		if _, ok := refusedFlags[f.Name]; ok {
			found = append(found, f.Name)
		}
	})
	sort.Strings(found)
	if len(found) == 0 {
		return nil
	}
	return refuseFlag("--"+found[0], refusedFlags[found[0]])
}

// silentlyIgnoredFlags is the list of pgBackRest knobs that
// have no semantic equivalent in pg_hardstorage but appear
// frequently in production configs.  We accept them so the
// command line parses, then ignore them.  An operator running
// the translator (`pg_hardstorage compat translate ...`) gets
// a stderr summary of every such flag in their config.
var silentlyIgnoredFlags = []string{
	"log-level-console",
	"log-level-file",
	"log-path",
	"process-max",
	"db-timeout",
	"protocol-timeout",
	"buffer-size",
}

// silentlyIgnoredBoolFlags are pgBackRest BOOLEAN knobs. pgBackRest
// accepts them as bare switches (`--start-fast`), so they must be
// registered as bool flags — a string flag would make cobra demand a
// value and fail to parse. They carry no semantic weight in the shim.
var silentlyIgnoredBoolFlags = []string{
	"start-fast",
	"stop-auto",
	"backup-standby",
}

// globalArgs holds the parsed root-level flags.  Sub-verbs
// read from this after cobra finishes parsing.  Tests reset
// it before each table case.
var globalArgs pgbackrestArgs

// resetGlobalArgs zeroes the shared args.  Tests call it
// before each table case so flags don't leak between runs.
func resetGlobalArgs() { globalArgs = pgbackrestArgs{} }

// mapToNativeArgs renders the union of pgBackRest flags into
// the slice of args we then feed to internal/cli.NewRoot()
// via SetArgs.  Returns warnings as a parallel slice so the
// caller can surface them on stderr (one line per warning,
// stable prefix `pg-hardstorage-pgbackrest: warn: ...`).
//
// The verb argument is what appears as the FIRST element of
// the returned slice and selects which subset of pgBackRest
// flags is meaningful.
func mapToNativeArgs(verb string, a pgbackrestArgs) (native []string, warnings []string, err error) {
	if a.stanza == "" {
		return nil, nil, fmt.Errorf("pg-hardstorage-pgbackrest: --stanza is required")
	}

	// `verb` may be a multi-word native verb ("wal push", "wal fetch")
	// so we can distinguish sub-verbs that differ in flag acceptance.
	// native[0] carries only the FIRST token; callers append the rest
	// of the positional shape themselves.
	head := verb
	if i := strings.IndexByte(verb, ' '); i >= 0 {
		head = verb[:i]
	}
	native = append(native, head)

	// Verb-specific positional arg shapes are appended by the
	// caller; here we only emit shared flags.

	// Only append --pg-connection for verbs that actually register it.
	// Native `restore`, `list` (info), `verify`, and `wal fetch`
	// (archive-get) do NOT define --pg-connection — passing it makes
	// cobra reject the argv as unknown. `backup` and `wal push`
	// (archive-push) do accept it.
	// Explicit flags win; the stanza's pg_hardstorage.yaml deployment
	// fills only what the command line left out. A real pgBackRest
	// cron carries just --stanza (everything else lives in
	// pgbackrest.conf), so without this fallback the canonical
	// invocation failed with usage.missing_flag.
	fromConfig := stanzaLookup(a.stanza)

	conn := buildPGConnection(a)
	if conn == "" {
		conn = fromConfig.PGConnection
	}
	if verbAcceptsPGConnection(verb) && conn != "" {
		native = append(native, "--pg-connection", conn)
	}

	repoURL, w, e := buildRepoURL(a)
	if e != nil {
		return nil, nil, e
	}
	if repoURL == "" {
		repoURL = fromConfig.Repo
	} else {
		warnings = append(warnings, w...)
	}
	if repoURL != "" {
		native = append(native, "--repo", repoURL)
	}

	// Say what is still missing HERE, naming the stanza and the way
	// out, rather than letting the native CLI report a bare
	// "--pg-connection, --repo are required" three layers down.
	var missing []string
	if verbAcceptsPGConnection(verb) && conn == "" {
		missing = append(missing, "--pg1-host (or pg_connection)")
	}
	if repoURL == "" && verbNeedsRepo(verb) {
		missing = append(missing, "--repo1-path (or repo)")
	}
	if len(missing) > 0 {
		return nil, nil, missingStanzaHint(a.stanza, missing)
	}

	// Cipher: pgBackRest's CBC vs our GCM is a real
	// algorithm difference.  We forward as a passphrase
	// (the native KEK-derivation path treats the value
	// as a passphrase) but surface a warning.
	if strings.EqualFold(a.repo1CipherType, "aes-256-cbc") && a.repo1CipherPass != "" {
		warnings = append(warnings,
			"warn: pgBackRest aes-256-cbc maps to native AES-256-GCM; algorithm differs but passphrase is honoured")
	}

	// Compression: native default is zstd.  Anything else
	// gets a warning.
	switch strings.ToLower(a.compressType) {
	case "", "zstd", "none":
		// quiet
	case "lz4", "gzip":
		warnings = append(warnings,
			fmt.Sprintf("warn: --compress-type=%s ignored; native uses zstd by default", a.compressType))
	}

	if a.archiveAsync {
		warnings = append(warnings,
			"warn: --archive-async ignored; native streaming is already async via the replication slot")
	}

	// --retention-full parsed into retentionFull and was then dropped
	// on the floor: no native arg, no warning. An operator's
	// `backup --retention-full=7` appeared to "run unchanged" while
	// retention silently reverted to the native default. Retention is
	// applied by `rotate`, not by `backup`, so the shim cannot forward
	// it — but it must never pretend it did.
	if a.retentionFull > 0 {
		warnings = append(warnings,
			fmt.Sprintf("warn: --retention-full=%d is NOT applied by this command; "+
				"pg_hardstorage applies retention in `rotate` — set `retention: {policy: count, keep_fulls: %d}` "+
				"for this deployment in pg_hardstorage.yaml, or run "+
				"`pg_hardstorage rotate %s --policy count --keep-fulls %d --apply`",
				a.retentionFull, a.retentionFull, a.stanza, a.retentionFull))
	}

	return native, warnings, nil
}

// verbAcceptsPGConnection reports whether the native verb registers a
// --pg-connection flag. Only `backup` and `wal push` (archive-push)
// do; `restore`, `list` (info), `verify`, and `wal fetch`
// (archive-get) reject it — they operate on the repository, not a
// live PG endpoint.
func verbAcceptsPGConnection(verb string) bool {
	switch verb {
	case "backup", "wal push":
		return true
	default:
		return false
	}
}

// verbNeedsRepo reports whether the native verb cannot run without a
// repository. Every verb the shim dispatches reads or writes the
// repo; `check` is the one that can still say something useful
// without it, so it is not gated.
func verbNeedsRepo(verb string) bool {
	switch verb {
	case "doctor":
		return false
	default:
		return true
	}
}

// buildPGConnection assembles a libpq URI from the four
// pg1-* flags.  Empty inputs yield an empty string so we
// don't pass a meaningless --pg-connection through.
func buildPGConnection(a pgbackrestArgs) string {
	if a.pg1Host == "" {
		return ""
	}
	user := a.pg1User
	if user == "" {
		user = "postgres"
	}
	host := a.pg1Host
	if a.pg1Port > 0 {
		host = fmt.Sprintf("%s:%d", a.pg1Host, a.pg1Port)
	}
	db := a.pg1Database
	if db == "" {
		db = "postgres"
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.User(user),
		Host:   host,
		Path:   "/" + db,
	}
	return u.String()
}

// buildRepoURL renders --repo for the native CLI.  Returns
// the empty string when no repo is configured (some shim
// invocations — e.g. `info` against a stanza-less tree —
// don't need it).  Warnings carry the cred-by-env reminder
// for S3.
func buildRepoURL(a pgbackrestArgs) (string, []string, error) {
	switch strings.ToLower(a.repo1Type) {
	// "file" is pgBackRest's own documented spelling for a local
	// repository and the value most real pgbackrest.conf files carry;
	// "posix" is what its option validator echoes back. Both mean the
	// same thing. Rejecting "file" made the single most common repo
	// shape unusable through the shim.
	case "posix", "file", "":
		// Empty type with --repo1-path set: treat as posix.
		if a.repo1Path == "" {
			return "", nil, nil
		}
		// Native expects file:// + absolute path.
		if !strings.HasPrefix(a.repo1Path, "/") {
			return "", nil, fmt.Errorf(
				"pg-hardstorage-pgbackrest: --repo1-path must be absolute (got %q)",
				a.repo1Path)
		}
		return "file://" + a.repo1Path, nil, nil

	case "s3":
		if a.repo1S3Bucket == "" {
			return "", nil, fmt.Errorf(
				"pg-hardstorage-pgbackrest: --repo1-s3-bucket required when --repo1-type=s3")
		}
		// Optional path component: pgBackRest also has a
		// --repo1-path that, with type=s3, is the prefix
		// inside the bucket.
		path := a.repo1S3Bucket
		if prefix := strings.TrimLeft(a.repo1Path, "/"); prefix != "" {
			path = path + "/" + prefix
		}
		warnings := []string{
			"warn: AWS credentials must be supplied via the standard SDK chain (env, IRSA, profile); --repo1-s3-key / --repo1-s3-key-secret are not honoured",
		}

		// Endpoint + region + path-style:  the native S3
		// storage plugin accepts these as URL query params
		// (?endpoint=...&region=...&path_style=true).  Without
		// them, the SDK targets real AWS, which is wrong for
		// MinIO / R2 / Wasabi / any S3-compat endpoint.
		// path_style=true is forced whenever a custom endpoint
		// is set — vhost addressing (bucket.<endpoint>) only
		// works against real AWS where DNS resolves the
		// per-bucket hostname.
		params := []string{}
		if a.repo1S3Endpoint != "" {
			params = append(params, "endpoint="+a.repo1S3Endpoint)
			params = append(params, "path_style=true")
		}
		if a.repo1S3Region != "" {
			params = append(params, "region="+a.repo1S3Region)
		}
		out := "s3://" + path
		if len(params) > 0 {
			out += "?" + strings.Join(params, "&")
		}
		return out, warnings, nil

	default:
		return "", nil, fmt.Errorf(
			"pg-hardstorage-pgbackrest: unsupported --repo1-type %q (supported: posix, file, s3)",
			a.repo1Type)
	}
}

// emitWarnings writes one line per warning to stderr.  Verb
// runners call this just before dispatching the native CLI.
func emitWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintln(stderrWriter, "pg-hardstorage-pgbackrest:", w)
	}
}
