// flags.go — WAL-G shim env-var collector + WALG_*/PG* → native arg translator (repo prefix, libpq, encryption refusals).
package walg

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// walgEnv is the set of WAL-G env vars + the small number of CLI
// flags we honour.  Each verb builds its slice of native CLI args
// via mapEnvToNativeArgs.
type walgEnv struct {
	// Repo prefix (mutually exclusive)
	s3Prefix    string // WALG_S3_PREFIX
	gsPrefix    string // WALG_GS_PREFIX
	azurePrefix string // WALG_AZURE_PREFIX
	filePrefix  string // WALG_FILE_PREFIX
	sshPrefix   string // WALG_SSH_PREFIX

	// PG endpoint (libpq env vars; WAL-G inherits them)
	pgHost     string // PGHOST
	pgPort     string // PGPORT
	pgUser     string // PGUSER
	pgDatabase string // PGDATABASE
	// PGPASSWORD intentionally not handled — never inline it.

	// pg_hardstorage extension: optional explicit deployment name.
	// WAL-G has no stanza concept, so a single host can only host
	// one repo.  The deployment name defaults to PGHOST (or
	// "default") if absent.
	deployment string // PG_HARDSTORAGE_DEPLOYMENT

	// Encryption refusals (mapped to remediation, not silently
	// honoured — the algorithms differ).
	libsodiumKey string // WALG_LIBSODIUM_KEY
	gpgKeyID     string // WALG_GPG_KEY_ID
	pgpKey       string // WALG_PGP_KEY
	pgpKeyPath   string // WALG_PGP_KEY_PATH

	// Soft compatibility (warned-and-ignored).
	compressionMethod string // WALG_COMPRESSION_METHOD
	tarSizeThreshold  string // WALG_TAR_SIZE_THRESHOLD
	deltaMaxSteps     string // WALG_DELTA_MAX_STEPS — silently honoured

	// Endpoint override (S3-compatible: MinIO, Wasabi, etc.).
	awsEndpoint string // AWS_ENDPOINT

	// awsForcePathStyle ("true" / "false") forces path-style
	// addressing on the SDK.  Required for any S3 endpoint that
	// doesn't resolve <bucket>.<host> via DNS (MinIO loopback,
	// localstack).  WAL-G convention is AWS_S3_FORCE_PATH_STYLE;
	// the native CLI reads `path_style=true` from the URL query.
	awsForcePathStyle string // AWS_S3_FORCE_PATH_STYLE

	// awsRegion is forwarded into the URL as ?region=… when
	// non-empty.  AWS_REGION is the cross-tool standard env var;
	// the SDK already reads it, but we also pin it on the URL so
	// `pg_hardstorage doctor` can attribute the source.
	awsRegion string // AWS_REGION
}

// loadEnv reads the environment.  Tests inject a fake by setting
// envLookup; default reads os.Getenv.
var envLookup = os.Getenv

func loadEnv() walgEnv {
	return walgEnv{
		s3Prefix:    envLookup("WALG_S3_PREFIX"),
		gsPrefix:    envLookup("WALG_GS_PREFIX"),
		azurePrefix: envLookup("WALG_AZURE_PREFIX"),
		filePrefix:  envLookup("WALG_FILE_PREFIX"),
		sshPrefix:   envLookup("WALG_SSH_PREFIX"),

		pgHost:     envLookup("PGHOST"),
		pgPort:     envLookup("PGPORT"),
		pgUser:     envLookup("PGUSER"),
		pgDatabase: envLookup("PGDATABASE"),

		deployment: envLookup("PG_HARDSTORAGE_DEPLOYMENT"),

		libsodiumKey: envLookup("WALG_LIBSODIUM_KEY"),
		gpgKeyID:     envLookup("WALG_GPG_KEY_ID"),
		pgpKey:       envLookup("WALG_PGP_KEY"),
		pgpKeyPath:   envLookup("WALG_PGP_KEY_PATH"),

		compressionMethod: envLookup("WALG_COMPRESSION_METHOD"),
		tarSizeThreshold:  envLookup("WALG_TAR_SIZE_THRESHOLD"),
		deltaMaxSteps:     envLookup("WALG_DELTA_MAX_STEPS"),

		awsEndpoint:       envLookup("AWS_ENDPOINT"),
		awsForcePathStyle: envLookup("AWS_S3_FORCE_PATH_STYLE"),
		awsRegion:         envLookup("AWS_REGION"),
	}
}

// deploymentName resolves the deployment label for native CLI
// dispatch.  Precedence: PG_HARDSTORAGE_DEPLOYMENT, PGHOST,
// "default".  WAL-G itself has no equivalent because every host
// owns exactly one repo prefix; the native CLI requires a
// deployment name as the first positional, so we synthesise one.
func (e walgEnv) deploymentName() string {
	if e.deployment != "" {
		return e.deployment
	}
	if e.pgHost != "" {
		// Use the bare host (strip any port artefacts that snuck in).
		host := e.pgHost
		if i := strings.IndexAny(host, ":/"); i >= 0 {
			host = host[:i]
		}
		return host
	}
	return "default"
}

// mapEnvToNativeArgs renders the union of WAL-G env vars into the
// shared --pg-connection / --repo arguments for the native CLI.
// Returns warnings as a parallel slice so callers can surface them
// on stderr.  The verb arg appears as the FIRST element of the
// returned slice; per-verb positionals are appended by the caller.
func mapEnvToNativeArgs(verb string, e walgEnv) (native []string, warnings []string, err error) {
	native = append(native, verb)

	if conn := buildPGConnection(e); conn != "" {
		native = append(native, "--pg-connection", conn)
	}
	repo, repoWarn, repoErr := buildRepoURL(e)
	if repoErr != nil {
		return nil, nil, repoErr
	}
	if repo != "" {
		native = append(native, "--repo", repo)
		warnings = append(warnings, repoWarn...)
	}

	// Encryption refusals — these are hard errors, not warnings:
	// WAL-G's libsodium / GPG / PGP envelopes are not byte-
	// compatible with our envelope, so silently re-encrypting
	// would mislead operators relying on the env var to choose
	// algorithms.
	if e.libsodiumKey != "" {
		return nil, nil, fmt.Errorf(
			"pg-hardstorage-walg: WALG_LIBSODIUM_KEY is not honoured (libsodium box is not byte-compatible with native AES-256-GCM); configure encryption.kek_ref in pg_hardstorage.yaml")
	}
	if e.gpgKeyID != "" || e.pgpKey != "" || e.pgpKeyPath != "" {
		return nil, nil, fmt.Errorf(
			"pg-hardstorage-walg: WAL-G GPG/PGP envelope is not honoured; configure encryption.kek_ref (KMS-wrapped DEK) in pg_hardstorage.yaml")
	}

	// Soft warnings.
	switch strings.ToLower(e.compressionMethod) {
	case "", "zstd":
		// quiet — native default.
	case "lz4", "lzma", "brotli":
		warnings = append(warnings,
			fmt.Sprintf("warn: WALG_COMPRESSION_METHOD=%s ignored; native uses zstd by default",
				e.compressionMethod))
	}
	if e.tarSizeThreshold != "" {
		warnings = append(warnings,
			"warn: WALG_TAR_SIZE_THRESHOLD ignored; native uses content-defined chunking, not tar splitting")
	}
	// deltaMaxSteps is silently honoured — native rolls back to
	// the nearest full automatically.

	// AWS_ENDPOINT is now threaded into --repo s3://...?endpoint=...
	// by buildRepoURL — no warning needed.

	return native, warnings, nil
}

// buildPGConnection assembles a libpq URI from the standard PG env
// vars.  Empty PGHOST yields an empty string so we don't pass a
// meaningless --pg-connection through.
func buildPGConnection(e walgEnv) string {
	if e.pgHost == "" {
		return ""
	}
	user := e.pgUser
	if user == "" {
		user = "postgres"
	}
	host := e.pgHost
	if e.pgPort != "" {
		host = e.pgHost + ":" + e.pgPort
	}
	db := e.pgDatabase
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

// buildRepoURL renders --repo for the native CLI.  Exactly one of
// the WALG_*_PREFIX variables must be set; multiple is an error.
// SSH prefix maps to sftp:// with a notice.
//
// For S3 prefixes: AWS_ENDPOINT is woven into the URL as
// `?endpoint=...` so the native S3 storage plugin's
// BaseEndpoint override fires (the plugin reads the URL query,
// NOT the env var directly).  AWS_S3_FORCE_PATH_STYLE=true is
// the WAL-G convention — also forced under any custom
// endpoint, since vhost addressing fails against MinIO / R2 /
// other non-AWS DNS.  Without this threading the SDK targets
// real AWS (e.g. testkit.s3.us-east-1.amazonaws.com), which
// silently fails with NoSuchBucket against an account that
// doesn't own the bucket.
func buildRepoURL(e walgEnv) (string, []string, error) {
	prefixes := 0
	for _, p := range []string{e.s3Prefix, e.gsPrefix, e.azurePrefix, e.filePrefix, e.sshPrefix} {
		if p != "" {
			prefixes++
		}
	}
	if prefixes > 1 {
		return "", nil, fmt.Errorf(
			"pg-hardstorage-walg: multiple WALG_*_PREFIX vars set; pick exactly one")
	}

	switch {
	case e.s3Prefix != "":
		warnings := []string{
			"warn: AWS credentials must be supplied via the standard SDK chain (env, IRSA, profile)",
		}
		// Compose ?endpoint=…&path_style=true&region=… onto
		// the bare WALG_S3_PREFIX.  Operators using MinIO /
		// R2 / Wasabi need this; without it the native CLI's
		// AWS SDK targets real AWS.
		params := []string{}
		if e.awsEndpoint != "" {
			params = append(params, "endpoint="+e.awsEndpoint)
			params = append(params, "path_style=true") // forced under any custom endpoint
		} else if strings.EqualFold(e.awsForcePathStyle, "true") {
			params = append(params, "path_style=true")
		}
		if e.awsRegion != "" {
			params = append(params, "region="+e.awsRegion)
		}
		out := e.s3Prefix
		// Honour any pre-existing query string the operator
		// pinned in WALG_S3_PREFIX itself (rare but legal).
		separator := "?"
		if strings.Contains(out, "?") {
			separator = "&"
		}
		if len(params) > 0 {
			out += separator + strings.Join(params, "&")
		}
		return out, warnings, nil
	case e.gsPrefix != "":
		return e.gsPrefix, nil, nil
	case e.azurePrefix != "":
		return e.azurePrefix, nil, nil
	case e.filePrefix != "":
		// file:// + absolute path.
		if !strings.HasPrefix(e.filePrefix, "/") {
			return "", nil, fmt.Errorf(
				"pg-hardstorage-walg: WALG_FILE_PREFIX must be absolute (got %q)",
				e.filePrefix)
		}
		return "file://" + e.filePrefix, nil, nil
	case e.sshPrefix != "":
		// ssh://user@host:/path → sftp://user@host:/path.
		mapped := strings.Replace(e.sshPrefix, "ssh://", "sftp://", 1)
		return mapped, []string{
			"warn: WALG_SSH_PREFIX mapped to sftp:// — verify with `pg_hardstorage doctor`",
		}, nil
	}
	return "", nil, nil
}

// emitWarnings writes one line per warning to stderr.  Verb runners
// call this just before dispatching the native CLI.  Tests
// substitute a *bytes.Buffer; production runs against os.Stderr.
var stderrWriter io.Writer = os.Stderr

func emitWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintln(stderrWriter, "pg-hardstorage-walg:", w)
	}
}
