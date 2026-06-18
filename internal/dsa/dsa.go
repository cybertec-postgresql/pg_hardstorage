// Package dsa implements the GDPR Data Subject Access (DSA) helper:
// "given a subject ID, locate which backups contain their data."
// Closes the SPEC commitment for the DSA helper that pairs
// with `kms shred` to deliver end-to-end Article 15 (right of
// access) and Article 17 (right to erasure / "right to be
// forgotten") workflows.
//
// Design: pg_hardstorage cannot peek inside encrypted chunk content
// (and shouldn't — the threat model puts the operator's keystore
// outside our trust boundary).  But the *tenant boundary* is the
// natural unit of GDPR compliance: each tenant has its own KEK,
// and `kms shred` operates at tenant granularity.  So we locate
// affected backups by walking manifests filtered by tenant.
//
// The operator supplies an opaque `subject_id` (a customer UUID,
// hashed email, internal user ID, etc.) and a `tenant` that
// contains that subject.  The mapping from `subject_id` to tenant
// is the operator's responsibility: pg_hardstorage doesn't see
// raw subject data so it can't derive the mapping itself.  The
// report records both pieces so an auditor can later confirm the
// operator looked up the correct tenant.
//
// Output: a signed Report with the schema
// `pg_hardstorage.dsa.report.v1`.  The Report enumerates every
// affected backup, the encryption ref (so the operator knows
// which KEK to shred), and a suggested-action block.  Reports
// are persisted at `dsa/reports/<id>.json` so an Article 15
// disclosure can later be cited and re-verified.
//
// Privacy note: we hash the raw subject_id with SHA-256 before
// recording it in the report.  The hash is a stable identifier
// for cross-referencing related reports, but the raw ID is not
// stored in the chain — operators who need the raw ID for an
// out-of-band disclosure record it elsewhere (e.g. their ticket
// system).
package dsa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/backup"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/storage"
)

// Schema strings (24-month backward-compat).
const (
	SchemaReport      = "pg_hardstorage.dsa.report.v1"
	canonicalSigInput = "pg_hardstorage.dsa.report.canon.v1"
)

// Article enumerates the GDPR articles a request can serve.
type Article string

const (
	// ArticleAccess is GDPR Article 15 — right of access.
	ArticleAccess Article = "art_15_access"
	// ArticleErasure is GDPR Article 17 — right to erasure.
	ArticleErasure Article = "art_17_erasure"
	// ArticleOther covers operator-defined articles (e.g. Art. 16
	// rectification) handled outside the standard two.
	ArticleOther Article = "other"
)

// AffectedBackup is one backup containing the subject's data
// (because it carries the named tenant).
type AffectedBackup struct {
	Deployment string    `json:"deployment"`
	BackupID   string    `json:"backup_id"`
	Type       string    `json:"type"`
	Tenant     string    `json:"tenant"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at"`
	Encrypted  bool      `json:"encrypted"`
	KEKRef     string    `json:"kek_ref,omitempty"`
	WrappedDEK string    `json:"wrapped_dek,omitempty"`
	FileCount  int       `json:"file_count"`
	ChunkCount int       `json:"chunk_count"`
}

// AffectedDeployment is the per-deployment rollup.
type AffectedDeployment struct {
	Deployment  string   `json:"deployment"`
	BackupCount int      `json:"backup_count"`
	BackupIDs   []string `json:"backup_ids,omitempty"`
}

// SuggestedAction tells the operator what to do next.
type SuggestedAction struct {
	Article     Article `json:"article"`
	Description string  `json:"description"`
	Command     string  `json:"command,omitempty"`
	DocURL      string  `json:"doc_url,omitempty"`
}

// Report is the signed DSA disclosure body.
type Report struct {
	Schema      string    `json:"schema"`
	ID          string    `json:"id"`
	GeneratedAt time.Time `json:"generated_at"`

	// SubjectIDHash is the SHA-256 hex of the raw subject_id.  We
	// don't store the raw ID (could be a real identifier).  An
	// auditor verifies the report against the same raw ID by
	// re-hashing.
	SubjectIDHash string `json:"subject_id_hash"`

	// Tenant is the GDPR-compliance boundary that contains this
	// subject's data.
	Tenant string `json:"tenant"`

	// Article identifies which GDPR article the request serves.
	Article Article `json:"article"`

	// Note: operator-supplied free-text justification (e.g. ticket
	// reference, request date).
	Note string `json:"note,omitempty"`

	// Window: optional time range the locate scanned.
	WindowStart *time.Time `json:"window_start,omitempty"`
	WindowEnd   *time.Time `json:"window_end,omitempty"`

	// Counters.
	ManifestsScanned    int `json:"manifests_scanned"`
	ManifestsAffected   int `json:"manifests_affected"`
	DeploymentsAffected int `json:"deployments_affected"`

	// Per-deployment rollup.
	Deployments []AffectedDeployment `json:"deployments,omitempty"`

	// Per-backup detail.  Sorted by StartedAt (oldest first) so the
	// chronology of the subject's footprint is obvious.
	AffectedBackups []AffectedBackup `json:"affected_backups,omitempty"`

	// SuggestedActions is the action plan the operator presents to
	// the regulator: under Article 17, run kms shred against the
	// named tenant; under Article 15, extract data per-backup.
	SuggestedActions []SuggestedAction `json:"suggested_actions,omitempty"`

	// Signed-attestation block.
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
	BodyHash             string `json:"body_hash,omitempty"`
	Signature            string `json:"signature,omitempty"`
}

// Sentinel errors.
var (
	ErrReportNotFound    = errors.New("dsa: report not found")
	ErrSignatureInvalid  = errors.New("dsa: signature does not validate")
	ErrSubjectIDRequired = errors.New("dsa: subject_id is required")
	ErrTenantRequired    = errors.New("dsa: tenant is required")
	ErrArticleRequired   = errors.New("dsa: article is required")
	ErrInvalidArticle    = errors.New("dsa: invalid article")
)

// Signer is the signing surface (backup.Signer satisfies it).
type Signer interface {
	Sign(payload []byte) []byte
	PublicKey() ed25519.PublicKey
}

// KeyResolver looks up the public key for verification.
type KeyResolver interface {
	PublicKey(fingerprint string) (ed25519.PublicKey, error)
}

// SingleKeyResolver wraps one ed25519.PublicKey for the
// "I trust this key" verifier path.
type SingleKeyResolver struct {
	Key ed25519.PublicKey
}

// PublicKey returns the wrapped key regardless of fingerprint.
func (r *SingleKeyResolver) PublicKey(string) (ed25519.PublicKey, error) {
	if r.Key == nil {
		return nil, errors.New("dsa: no key configured")
	}
	return r.Key, nil
}

// LocateOptions drives one Locate call.
type LocateOptions struct {
	SubjectID string  // raw subject identifier; hashed before recording
	Tenant    string  // GDPR compliance boundary
	Article   Article // ArticleAccess | ArticleErasure | ArticleOther
	Note      string  // operator note (ticket reference, etc.)

	// Optional window.
	WindowStart time.Time
	WindowEnd   time.Time

	// Optional deployment scope.  Empty → all deployments.
	Deployment string

	// Now overrides time.Now (deterministic tests).
	Now func() time.Time
}

// Locator drives one DSA locate pass.
type Locator struct {
	manifests *backup.ManifestStore
	verifier  *backup.Verifier
}

// NewLocator constructs a Locator.
func NewLocator(manifests *backup.ManifestStore, verifier *backup.Verifier) *Locator {
	return &Locator{manifests: manifests, verifier: verifier}
}

// Locate walks the manifests, filters by tenant + (optional)
// deployment + (optional) window, and returns an unsigned Report.
// Caller signs via SignReport before persisting.
func (l *Locator) Locate(ctx context.Context, opts LocateOptions) (*Report, error) {
	if opts.SubjectID == "" {
		return nil, ErrSubjectIDRequired
	}
	if opts.Tenant == "" {
		return nil, ErrTenantRequired
	}
	if opts.Article == "" {
		return nil, ErrArticleRequired
	}
	switch opts.Article {
	case ArticleAccess, ArticleErasure, ArticleOther:
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidArticle, opts.Article)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	report := &Report{
		Schema:        SchemaReport,
		GeneratedAt:   now().UTC(),
		SubjectIDHash: hashSubjectID(opts.SubjectID),
		Tenant:        opts.Tenant,
		Article:       opts.Article,
		Note:          opts.Note,
	}
	report.ID = newReportID(report.GeneratedAt, opts.Tenant)
	if !opts.WindowStart.IsZero() {
		ws := opts.WindowStart.UTC()
		report.WindowStart = &ws
	}
	if !opts.WindowEnd.IsZero() {
		we := opts.WindowEnd.UTC()
		report.WindowEnd = &we
	}

	// Resolve which deployments to scan.
	var deployments []string
	if opts.Deployment != "" {
		deployments = []string{opts.Deployment}
	} else {
		ds, err := l.manifests.Deployments(ctx)
		if err != nil {
			return nil, fmt.Errorf("dsa: list deployments: %w", err)
		}
		deployments = ds
	}

	deploymentRollup := make(map[string]*AffectedDeployment)

	for _, d := range deployments {
		for m, err := range l.manifests.List(ctx, d, l.verifier) {
			if err != nil {
				// One bad manifest doesn't kill the whole scan; but
				// we record it (the operator should investigate).
				continue
			}
			report.ManifestsScanned++
			if m.Tenant != opts.Tenant {
				continue
			}
			if !inWindow(m.StoppedAt, opts.WindowStart, opts.WindowEnd) {
				continue
			}
			report.ManifestsAffected++
			ab := manifestToAffected(m)
			report.AffectedBackups = append(report.AffectedBackups, ab)

			rd, ok := deploymentRollup[d]
			if !ok {
				rd = &AffectedDeployment{Deployment: d}
				deploymentRollup[d] = rd
			}
			rd.BackupCount++
			rd.BackupIDs = append(rd.BackupIDs, m.BackupID)
		}
	}

	// Materialise the rollup, sorted by deployment for stable
	// output.
	for _, rd := range deploymentRollup {
		sort.Strings(rd.BackupIDs)
		report.Deployments = append(report.Deployments, *rd)
	}
	sort.Slice(report.Deployments, func(i, j int) bool {
		return report.Deployments[i].Deployment < report.Deployments[j].Deployment
	})
	report.DeploymentsAffected = len(report.Deployments)

	// Per-backup detail sorted oldest-first so the timeline reads
	// naturally.
	sort.Slice(report.AffectedBackups, func(i, j int) bool {
		return report.AffectedBackups[i].StartedAt.Before(report.AffectedBackups[j].StartedAt)
	})

	report.SuggestedActions = suggestedActionsFor(opts.Article, opts.Tenant)
	return report, nil
}

// manifestToAffected projects a Manifest into the report shape.
func manifestToAffected(m *backup.Manifest) AffectedBackup {
	ab := AffectedBackup{
		Deployment: m.Deployment,
		BackupID:   m.BackupID,
		Type:       string(m.Type),
		Tenant:     m.Tenant,
		StartedAt:  m.StartedAt,
		StoppedAt:  m.StoppedAt,
		FileCount:  len(m.Files),
	}
	for _, f := range m.Files {
		ab.ChunkCount += len(f.Chunks)
	}
	if m.Encryption != nil {
		ab.Encrypted = true
		ab.KEKRef = m.Encryption.KEKRef
		ab.WrappedDEK = m.Encryption.WrappedDEK
	}
	return ab
}

// suggestedActionsFor produces the per-Article action plan.
func suggestedActionsFor(article Article, tenant string) []SuggestedAction {
	switch article {
	case ArticleErasure:
		return []SuggestedAction{
			{
				Article:     ArticleErasure,
				Description: "Crypto-shred the tenant's KEK to render every affected backup unrecoverable.  After successful shred, the data is mathematically inaccessible even though the on-disk bytes remain.",
				Command:     fmt.Sprintf("pg_hardstorage kms shred --tenant %s", tenant),
				DocURL:      "https://docs.pghardstorage.org/runbooks/gdpr-art-17-erasure",
			},
			{
				Article:     ArticleOther,
				Description: "Record the request + this report in the operator's data-protection log; archive the report ID for the audit trail.",
				Command:     "pg_hardstorage dsa show <report-id> --repo <repo>",
			},
		}
	case ArticleAccess:
		return []SuggestedAction{
			{
				Article:     ArticleAccess,
				Description: "For each affected backup, perform a partial restore scoped to the tables containing the subject's data, then extract the rows via pg_dump or SQL filter.",
				Command:     "pg_hardstorage partial restore <deployment> --tables <list> --backup <id>",
				DocURL:      "https://docs.pghardstorage.org/runbooks/gdpr-art-15-access",
			},
		}
	}
	return []SuggestedAction{
		{
			Article:     ArticleOther,
			Description: "No standard action plan; the operator records the report and proceeds out-of-band.",
		},
	}
}

// inWindow returns true when t is in [start, end].  Either bound
// being zero means open-ended on that side.
func inWindow(t, start, end time.Time) bool {
	if !start.IsZero() && t.Before(start.UTC()) {
		return false
	}
	if !end.IsZero() && t.After(end.UTC()) {
		return false
	}
	return true
}

// hashSubjectID returns the SHA-256 hex of the raw subject_id.
// We use SHA-256 not because it's a strong privacy hash (it's not
// — short subject_id spaces are vulnerable to brute force) but
// because it's a stable cross-reference identifier.  Operators
// who care about offline-resistance use HMAC-SHA-256 with a
// per-tenant secret; that's an out-of-band hardening they apply
// before passing the subject_id to this command.
func hashSubjectID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// canonicalReportBytes is the byte sequence the operator's
// signature covers.  Length-prefixed; deterministic across Go
// runtimes; commits to every counter + the affected-backup digest.
func canonicalReportBytes(r *Report) []byte {
	var buf strings.Builder
	buf.WriteString(canonicalSigInput)
	buf.WriteByte(0)
	for _, field := range []string{
		r.Schema,
		r.ID,
		r.SubjectIDHash,
		r.Tenant,
		string(r.Article),
		r.Note,
	} {
		binary.Write(&buf, binary.BigEndian, int64(len(field)))
		buf.WriteString(field)
	}
	binary.Write(&buf, binary.BigEndian, r.GeneratedAt.UTC().UnixNano())
	if r.WindowStart != nil {
		binary.Write(&buf, binary.BigEndian, r.WindowStart.UTC().UnixNano())
	} else {
		binary.Write(&buf, binary.BigEndian, int64(0))
	}
	if r.WindowEnd != nil {
		binary.Write(&buf, binary.BigEndian, r.WindowEnd.UTC().UnixNano())
	} else {
		binary.Write(&buf, binary.BigEndian, int64(0))
	}
	binary.Write(&buf, binary.BigEndian, int64(r.ManifestsScanned))
	binary.Write(&buf, binary.BigEndian, int64(r.ManifestsAffected))
	binary.Write(&buf, binary.BigEndian, int64(r.DeploymentsAffected))
	digest := digestAffected(r)
	buf.Write(digest[:])
	return []byte(buf.String())
}

// digestAffected hashes the affected-backup list in stable order.
func digestAffected(r *Report) [32]byte {
	type item struct{ Deployment, BackupID, KEKRef string }
	items := make([]item, 0, len(r.AffectedBackups))
	for _, ab := range r.AffectedBackups {
		items = append(items, item{ab.Deployment, ab.BackupID, ab.KEKRef})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Deployment != items[j].Deployment {
			return items[i].Deployment < items[j].Deployment
		}
		return items[i].BackupID < items[j].BackupID
	})
	hash := sha256.New()
	for _, it := range items {
		fmt.Fprintf(hash, "%s|%s|%s\n", it.Deployment, it.BackupID, it.KEKRef)
	}
	var out [32]byte
	hash.Sum(out[:0])
	return out
}

// SignReport signs the report.  Mutates PublicKeyFingerprint +
// BodyHash + Signature.
func SignReport(r *Report, signer Signer) error {
	if signer == nil {
		return errors.New("dsa: nil signer")
	}
	r.PublicKeyFingerprint = publicKeyFingerprint(signer.PublicKey())
	canon := canonicalReportBytes(r)
	bodyHash := sha256.Sum256(canon)
	r.BodyHash = hex.EncodeToString(bodyHash[:])
	r.Signature = base64.StdEncoding.EncodeToString(signer.Sign(canon))
	return nil
}

// VerifyReport re-derives the canonical bytes, cross-checks
// BodyHash, and validates the ed25519 signature.
func VerifyReport(r *Report, resolver KeyResolver) error {
	if r == nil {
		return errors.New("dsa: nil report")
	}
	if r.Signature == "" {
		return ErrSignatureInvalid
	}
	canon := canonicalReportBytes(r)
	bodyHash := sha256.Sum256(canon)
	if hex.EncodeToString(bodyHash[:]) != r.BodyHash {
		return fmt.Errorf("%w: body_hash drift", ErrSignatureInvalid)
	}
	pub, err := resolver.PublicKey(r.PublicKeyFingerprint)
	if err != nil {
		return fmt.Errorf("dsa: resolve public key: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("dsa: decode signature: %w", err)
	}
	if !ed25519.Verify(pub, canon, sig) {
		return ErrSignatureInvalid
	}
	return nil
}

// ----- ID + fingerprint helpers -----

func newReportID(at time.Time, tenant string) string {
	hasher := fnv.New32a()
	hasher.Write([]byte(at.UTC().Format(time.RFC3339Nano)))
	hasher.Write([]byte(tenant))
	short := fmt.Sprintf("%08x", hasher.Sum32())
	return fmt.Sprintf("%020d-%s", at.UTC().Unix(), short)
}

func publicKeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// ----- storage -----

// ReportStore writes + reads reports under dsa/reports/<id>.json.
type ReportStore struct {
	sp storage.StoragePlugin
}

// NewReportStore wraps sp.
func NewReportStore(sp storage.StoragePlugin) *ReportStore {
	return &ReportStore{sp: sp}
}

func reportKey(id string) string { return "dsa/reports/" + id + ".json" }

// Put persists a report.
func (s *ReportStore) Put(ctx context.Context, r *Report) error {
	body, err := stdjson.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	key := reportKey(r.ID)
	tmp := key + ".tmp." + randHex(8)
	if _, err := s.sp.Put(ctx, tmp, bytes.NewReader(body), storage.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		return fmt.Errorf("dsa: put tmp: %w", err)
	}
	if err := s.sp.RenameIfNotExists(ctx, tmp, key); err != nil {
		_ = s.sp.Delete(ctx, tmp)
		return fmt.Errorf("dsa: rename: %w", err)
	}
	return nil
}

// Get reads + decodes one report.
func (s *ReportStore) Get(ctx context.Context, id string) (*Report, error) {
	rd, err := s.sp.Get(ctx, reportKey(id))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrReportNotFound, id, err)
	}
	defer rd.Close()
	body, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("dsa: read: %w", err)
	}
	var r Report
	if err := stdjson.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("dsa: decode: %w", err)
	}
	return &r, nil
}

// ListFilter filters the List output.
type ListFilter struct {
	Since         *time.Time
	Tenant        string
	Article       Article
	SubjectIDHash string
}

// List returns every report newest-first matching the filter.
func (s *ReportStore) List(ctx context.Context, f ListFilter) ([]*Report, error) {
	const prefix = "dsa/reports/"
	var out []*Report
	for obj, err := range s.sp.List(ctx, prefix) {
		if err != nil {
			return nil, fmt.Errorf("dsa: list: %w", err)
		}
		base := path.Base(obj.Key)
		if !strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".tmp") {
			continue
		}
		id := strings.TrimSuffix(base, ".json")
		r, err := s.Get(ctx, id)
		if err != nil {
			continue
		}
		if f.Since != nil && r.GeneratedAt.Before(*f.Since) {
			continue
		}
		if f.Tenant != "" && r.Tenant != f.Tenant {
			continue
		}
		if f.Article != "" && r.Article != f.Article {
			continue
		}
		if f.SubjectIDHash != "" && r.SubjectIDHash != f.SubjectIDHash {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GeneratedAt.After(out[j].GeneratedAt)
	})
	return out, nil
}

// ----- helpers -----

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HashSubjectIDForFilter is exposed so the CLI can compute a hash
// for the --subject-id-hash list filter without re-importing
// crypto/sha256 + encoding/hex.
func HashSubjectIDForFilter(id string) string {
	return hashSubjectID(id)
}
