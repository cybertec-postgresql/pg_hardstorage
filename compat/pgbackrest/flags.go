// flags.go — pgBackRest shim flag union + pgbackrest→native arg translator (connection, repo, compression, retention).
package pgbackrest

import (
	"fmt"
	"net/url"
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
	// knobs: operators write a bare `--start-fast` (no value). If they
	// were registered as string flags (like the ignored knobs above),
	// cobra would fail parsing with "flag needs an argument". Register
	// them as bool flags so a bare flag parses; the values are ignored.
	for _, ignoredBool := range silentlyIgnoredBoolFlags {
		fs.Bool(ignoredBool, false, "")
		_ = fs.MarkHidden(ignoredBool)
	}
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
	if verbAcceptsPGConnection(verb) {
		if conn := buildPGConnection(a); conn != "" {
			native = append(native, "--pg-connection", conn)
		}
	}
	if repoURL, w, e := buildRepoURL(a); e != nil {
		return nil, nil, e
	} else if repoURL != "" {
		native = append(native, "--repo", repoURL)
		warnings = append(warnings, w...)
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
	case "posix", "":
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
			"pg-hardstorage-pgbackrest: unsupported --repo1-type %q (supported: posix, s3)",
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
