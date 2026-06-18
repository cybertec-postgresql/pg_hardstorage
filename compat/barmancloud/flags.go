// Package barmancloud is the compat shim for the
// `barman-cloud-*` family of binaries that ship inside the
// CloudNativePG postgres image.  CNPG's controller invokes
// four distinct binaries directly:
//
//	barman-cloud-backup         — full backup (Backup CRD)
//	barman-cloud-restore        — restore from a backup
//	barman-cloud-wal-archive    — archive_command target
//	barman-cloud-wal-restore    — replica's restore_command
//
// Each binary has its own argv shape — there's no sub-command
// dispatcher, so the multi-call binary `pg-hardstorage-compat`
// routes by argv[0] and runs the matching verb directly.
//
// Configuration is partly flags (--endpoint-url, --cloud-provider,
// --gzip), partly env (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
// AWS_S3_FORCE_PATH_STYLE, AWS_REGION).  We honour the same
// rendering rules our walg shim does — AWS_* env vars become
// query parameters on the native --repo URL — so the operator
// experience stays consistent across drop-ins.
//
// Why a separate package (compat/barmancloud) rather than
// folding into compat/barman?  The two upstream tools are
// distinct projects with different argv conventions:
// standalone Barman uses a server.conf file + `barman <verb>
// <server>`, while barman-cloud-* is the cloud-only Python
// family that takes everything on argv.  Mixing the two
// dispatchers would produce a confused shim that handles
// neither cleanly.
package barmancloud

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// envLookup is overridable in tests — same idiom the walg
// shim uses.
var envLookup = os.Getenv

// commonFlags is the flag set every barman-cloud-* binary
// shares.  Each verb's cobra command attaches it to its own
// command via attachCommonFlags; the parsed values come back
// through the bcEnv struct returned by readEnv.
type commonFlags struct {
	cloudProvider string // --cloud-provider {aws-s3|azure-blob-storage|google-cloud-storage}
	endpointURL   string // --endpoint-url
	gzip          bool   // --gzip
	user          string // --user (barman-cloud-backup only — PG role)
	name          string // --name  (barman-cloud-backup only — backup label)
	hostStanza    string // legacy --host-stanza (rarely used)
}

func attachCommonFlags(c *cobra.Command, f *commonFlags, withUserName bool) {
	c.Flags().StringVar(&f.cloudProvider, "cloud-provider", "aws-s3",
		"cloud provider: aws-s3 | azure-blob-storage | google-cloud-storage")
	c.Flags().StringVar(&f.endpointURL, "endpoint-url", "",
		"S3 endpoint URL (custom: MinIO, R2, Wasabi)")
	c.Flags().BoolVar(&f.gzip, "gzip", false,
		"compress; honoured at the native CLI's compression knob")
	if withUserName {
		c.Flags().StringVar(&f.user, "user", "", "PG role used to issue pg_basebackup")
		c.Flags().StringVar(&f.name, "name", "", "operator-assigned backup label")
	}
	// Soft-honoured flags that barman-cloud accepts but we
	// don't need to thread through (compression bands, parallel
	// jobs, encryption refusals, etc.) — define them here so
	// cobra doesn't reject the operator's full argv.  We log
	// them at the verb level only when they meaningfully
	// affect behaviour.
	c.Flags().Int("jobs", 0, "(silently honoured at native parallelism floor)")
	c.Flags().String("compression", "", "(silently mapped: gzip → balanced)")
	c.Flags().String("history", "", "(unused; barman-cloud's history dir)")
	c.Flags().String("tags", "", "(silently ignored)")
	c.Flags().String("encryption", "", "(refused at native KMS)")
	c.Flags().String("max-archive-size", "", "(silently honoured at native chunk floor)")
	c.Flags().String("min-chunk-size", "", "(silently ignored — native uses FastCDC)")
	c.Flags().String("read-timeout", "", "(silently honoured)")
	c.Flags().Bool("verbose", false, "(silently honoured)")
}

// bcEnv carries the AWS_* / WAL_* env vars the shim threads
// onto the native --repo URL.  Same shape as the walg shim's
// env helper.
type bcEnv struct {
	accessKey  string // AWS_ACCESS_KEY_ID — read for diagnostics; the native SDK consumes it
	secretKey  string // AWS_SECRET_ACCESS_KEY — same
	region     string // AWS_REGION
	forcePath  string // AWS_S3_FORCE_PATH_STYLE
	pgHost     string // PGHOST — used as fallback deployment label
	pgPort     string // PGPORT
	pgUser     string // PGUSER
	deployment string // PG_HARDSTORAGE_DEPLOYMENT (extension)
}

func readEnv() bcEnv {
	return bcEnv{
		accessKey:  envLookup("AWS_ACCESS_KEY_ID"),
		secretKey:  envLookup("AWS_SECRET_ACCESS_KEY"),
		region:     envLookup("AWS_REGION"),
		forcePath:  envLookup("AWS_S3_FORCE_PATH_STYLE"),
		pgHost:     envLookup("PGHOST"),
		pgPort:     envLookup("PGPORT"),
		pgUser:     envLookup("PGUSER"),
		deployment: envLookup("PG_HARDSTORAGE_DEPLOYMENT"),
	}
}

// deploymentName picks the deployment label the native CLI
// commands take as the first positional.  CNPG's stanza name
// passed by the operator is the canonical choice when present;
// PG_HARDSTORAGE_DEPLOYMENT overrides; otherwise PGHOST is the
// fallback (CNPG's PGHOST is the in-pod /controller/run socket
// dir which is fine for label use).
func (e bcEnv) deploymentName(stanza string) string {
	if e.deployment != "" {
		return e.deployment
	}
	if stanza != "" {
		return stanza
	}
	if e.pgHost != "" {
		// Strip artefacts: socket paths become "default", real
		// hostnames stay as the host.
		if strings.HasPrefix(e.pgHost, "/") {
			return "default"
		}
		return e.pgHost
	}
	return "default"
}

// buildRepoURL turns the operator's positional s3-path +
// flags + AWS_* env vars into a native pg_hardstorage repo
// URL.  Same logic the walg shim uses, factored separately so
// we don't depend on a sister package's internal helpers.
//
//	s3://bucket/prefix
//	    + --endpoint-url=http://minio:9000
//	    + --cloud-provider=aws-s3
//	    + AWS_S3_FORCE_PATH_STYLE=true
//	    + AWS_REGION=us-east-1
//	  → s3://bucket/prefix?endpoint=http://minio:9000&path_style=true&region=us-east-1
//
// Any AWS_ENDPOINT-style endpoint (custom S3) implies
// path_style=true; a custom endpoint with vhost addressing
// fails against MinIO / R2 / other non-AWS DNS.
func buildRepoURL(positional string, f commonFlags, e bcEnv) (string, error) {
	if positional == "" {
		return "", fmt.Errorf("pg-hardstorage-barmancloud: missing destination (positional s3:// path)")
	}
	if !strings.HasPrefix(positional, "s3://") &&
		!strings.HasPrefix(positional, "azure://") &&
		!strings.HasPrefix(positional, "gs://") {
		return "", fmt.Errorf("pg-hardstorage-barmancloud: destination %q must start with s3:// / azure:// / gs://",
			positional)
	}

	params := []string{}
	if f.endpointURL != "" {
		params = append(params, "endpoint="+url.QueryEscape(f.endpointURL))
		// Custom endpoint always implies path-style addressing.
		params = append(params, "path_style=true")
	} else if strings.EqualFold(e.forcePath, "true") {
		params = append(params, "path_style=true")
	}
	if e.region != "" {
		params = append(params, "region="+url.QueryEscape(e.region))
	}

	out := positional
	separator := "?"
	if strings.Contains(out, "?") {
		separator = "&"
	}
	if len(params) > 0 {
		out += separator + strings.Join(params, "&")
	}
	return out, nil
}

// emitWarn prints a one-line shim warning to stderr.  Used
// when soft-honoured flags or env vars don't translate
// cleanly.  The operator sees this in the operator's pod log
// (CNPG forwards barman-cloud-* stderr to the controller's
// structured log).
func emitWarn(stderr *cobra.Command, format string, args ...any) {
	if stderr != nil {
		fmt.Fprintf(stderr.ErrOrStderr(), "pg-hardstorage-barmancloud: warn: "+format+"\n", args...)
	}
}
