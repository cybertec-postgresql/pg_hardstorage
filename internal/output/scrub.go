// scrub.go — last-line-of-defence credential masking for error text.
package output

import "regexp"

// scrubbedValue replaces every credential ScrubSecrets finds.
const scrubbedValue = "****"

var (
	// scheme://user:password@host — the password may contain '@'.
	scrubURLUserinfo = regexp.MustCompile(`(://[^\s/:@]*):[^\s/]*@`)
	// key=value credentials in URL queries and libpq keyword DSNs.
	scrubKeyValue = regexp.MustCompile(`(?i)(\b(?:password|passwd|sslpassword|secret_key|secretkey|secret|token|sas_token|sig|signature|access_key|accesskey|api_key|apikey|x-amz-signature|x-amz-credential|x-amz-security-token)\s*=\s*)('(?:\\.|[^'])*'|[^&\s"']+)`)
)

// ScrubSecrets masks credentials embedded in free text: the password in
// a URL's userinfo (sftp://u:pw@h, s3://AK:SK@b), secret-bearing query
// parameters (?sig=, ?token=, X-Amz-Signature=...) and password= in a
// libpq keyword/value string. Error messages routinely quote the
// repository URL or a connection string verbatim, and those end up in
// terminals, CI logs, event sinks and the audit log.
func ScrubSecrets(s string) string {
	if s == "" {
		return s
	}
	s = scrubURLUserinfo.ReplaceAllString(s, "${1}:"+scrubbedValue+"@")
	s = scrubKeyValue.ReplaceAllString(s, "${1}"+scrubbedValue)
	return s
}
