// catalogs.go — built-in EN/DE/FR/JA message catalogs registered at init for translatable CLI strings.
package i18n

// Built-in catalogs. + ships a representative ~30-key
// catalog covering common doctor / restore / status / error
// messages in EN/DE/FR/JA.  Future commits expand the key set
// as new commands surface translatable strings.
//
// Translation policy:
//   - English is the source of truth; every other locale's
//     keys mirror English's set (missing keys fall back to EN
//     transparently).
//   - Translators preserve template fields ({{.X}}) verbatim
//     so the data layer doesn't change between locales.
//   - Punctuation conventions follow the target locale's norm:
//     French uses non-breaking-space before colons + question
//     marks, German uses »quotes« in some places, Japanese
//     uses 「」 for emphasis.
//   - Tone matches the operator persona: terse, technical,
//     no exclamation marks, no emojis (the CLI surface stays
//     plain).

func init() {
	Register("en", englishMessages)
	Register("de", germanMessages)
	Register("fr", frenchMessages)
	Register("ja", japaneseMessages)

	RegisterPlurals("en", englishPlurals)
	RegisterPlurals("de", germanPlurals)
	RegisterPlurals("fr", frenchPlurals)
	RegisterPlurals("ja", japanesePlurals)
}

// englishMessages is the source-of-truth catalog.  Every key in
// every other locale must mirror a key in this map.
var englishMessages = map[string]string{
	// ----- doctor -----
	"doctor.healthy":            "all clear",
	"doctor.attention_required": "needs attention",
	"doctor.last_backup_ago":    "last backup {{.Ago}} ago",
	"doctor.wal_lag":            "WAL lag {{.Seconds}}s",
	"doctor.repo_unreachable":   "repository unreachable: {{.URL}}",
	"doctor.kms_unreachable":    "KMS key unreachable",
	"doctor.suggested_fix":      "suggested fix: {{.Command}}",

	// ----- restore -----
	"restore.refuses_live_pg":     "refusing to restore over a live PostgreSQL data directory",
	"restore.refuses_non_empty":   "target directory is not empty; pass --force to overwrite",
	"restore.refuses_primary":     "refusing to restore on the current Patroni primary",
	"restore.preview_header":      "Preview — would restore the following:",
	"restore.preview_target":      "Target:        {{.TargetDir}}",
	"restore.preview_pg_version":  "PostgreSQL:    {{.Version}}",
	"restore.preview_source":      "Source backup: {{.BackupID}} ({{.Type}})",
	"restore.preview_rto":         "Estimated RTO: ~{{.Minutes}} minutes",
	"restore.preview_run_command": "Run with --confirm to execute.",
	"restore.completed":           "restore complete in {{.Duration}}",

	// ----- status -----
	"status.no_backups":          "no backups yet",
	"status.next_backup_at":      "next backup at {{.Time}}",
	"status.next_drill_at":       "next drill at {{.Time}}",
	"status.replication_active":  "active",
	"status.replication_lagging": "lagging ({{.Seconds}}s)",
	"status.replication_stalled": "stalled",

	// ----- common errors -----
	"err.repo_required":    "--repo is required",
	"err.target_required":  "--target is required",
	"err.invalid_lsn":      "invalid LSN: {{.Value}}",
	"err.invalid_time":     "invalid time: {{.Value}}",
	"err.invalid_severity": "invalid severity: {{.Value}}",
	"err.unknown_format":   "unknown output format: {{.Value}}",
}

var englishPlurals = map[string]map[Form]string{
	"backups.count": {
		FormOne:   "{{.N}} backup",
		FormOther: "{{.N}} backups",
	},
	"deployments.count": {
		FormOne:   "{{.N}} deployment",
		FormOther: "{{.N}} deployments",
	},
	"chunks.count": {
		FormOne:   "{{.N}} chunk",
		FormOther: "{{.N}} chunks",
	},
	"manifests.affected": {
		FormOne:   "{{.N}} manifest affected",
		FormOther: "{{.N}} manifests affected",
	},
}

// germanMessages — German translations.  Tone: technical,
// formal-impersonal (no Sie/du), aligned with PostgreSQL's own
// German error-message conventions.
var germanMessages = map[string]string{
	"doctor.healthy":            "alles in Ordnung",
	"doctor.attention_required": "erfordert Aufmerksamkeit",
	"doctor.last_backup_ago":    "letztes Backup vor {{.Ago}}",
	"doctor.wal_lag":            "WAL-Verzögerung {{.Seconds}}s",
	"doctor.repo_unreachable":   "Repository nicht erreichbar: {{.URL}}",
	"doctor.kms_unreachable":    "KMS-Schlüssel nicht erreichbar",
	"doctor.suggested_fix":      "Vorgeschlagene Lösung: {{.Command}}",

	"restore.refuses_live_pg":     "Verweigere Restore über ein laufendes PostgreSQL-Datenverzeichnis",
	"restore.refuses_non_empty":   "Zielverzeichnis ist nicht leer; --force zum Überschreiben",
	"restore.refuses_primary":     "Verweigere Restore auf dem aktuellen Patroni-Primary",
	"restore.preview_header":      "Vorschau — folgendes würde wiederhergestellt:",
	"restore.preview_target":      "Ziel:           {{.TargetDir}}",
	"restore.preview_pg_version":  "PostgreSQL:     {{.Version}}",
	"restore.preview_source":      "Quell-Backup:   {{.BackupID}} ({{.Type}})",
	"restore.preview_rto":         "Geschätzte RTO: ~{{.Minutes}} Minuten",
	"restore.preview_run_command": "Mit --confirm ausführen.",
	"restore.completed":           "Restore abgeschlossen in {{.Duration}}",

	"status.no_backups":          "noch keine Backups",
	"status.next_backup_at":      "nächstes Backup um {{.Time}}",
	"status.next_drill_at":       "nächste Probe um {{.Time}}",
	"status.replication_active":  "aktiv",
	"status.replication_lagging": "verzögert ({{.Seconds}}s)",
	"status.replication_stalled": "blockiert",

	"err.repo_required":    "--repo ist erforderlich",
	"err.target_required":  "--target ist erforderlich",
	"err.invalid_lsn":      "ungültige LSN: {{.Value}}",
	"err.invalid_time":     "ungültige Zeit: {{.Value}}",
	"err.invalid_severity": "ungültige Schweregrad-Stufe: {{.Value}}",
	"err.unknown_format":   "unbekanntes Ausgabeformat: {{.Value}}",
}

var germanPlurals = map[string]map[Form]string{
	"backups.count": {
		FormOne:   "{{.N}} Backup",
		FormOther: "{{.N}} Backups",
	},
	"deployments.count": {
		FormOne:   "{{.N}} Deployment",
		FormOther: "{{.N}} Deployments",
	},
	"chunks.count": {
		FormOne:   "{{.N}} Chunk",
		FormOther: "{{.N}} Chunks",
	},
	"manifests.affected": {
		FormOne:   "{{.N}} Manifest betroffen",
		FormOther: "{{.N}} Manifeste betroffen",
	},
}

// frenchMessages — French.  Note the non-breaking spaces
// ( ) before the colon in French typography; we use
// ASCII spaces in the catalog for portability and let the
// terminal renderer handle visual presentation.
var frenchMessages = map[string]string{
	"doctor.healthy":            "tout est en ordre",
	"doctor.attention_required": "attention requise",
	"doctor.last_backup_ago":    "dernière sauvegarde il y a {{.Ago}}",
	"doctor.wal_lag":            "retard WAL {{.Seconds}}s",
	"doctor.repo_unreachable":   "dépôt inaccessible : {{.URL}}",
	"doctor.kms_unreachable":    "clé KMS inaccessible",
	"doctor.suggested_fix":      "solution suggérée : {{.Command}}",

	"restore.refuses_live_pg":     "refus de restauration sur un répertoire PostgreSQL actif",
	"restore.refuses_non_empty":   "le répertoire cible n'est pas vide ; passez --force pour écraser",
	"restore.refuses_primary":     "refus de restauration sur le primary Patroni actuel",
	"restore.preview_header":      "Aperçu — restauration prévue :",
	"restore.preview_target":      "Cible :          {{.TargetDir}}",
	"restore.preview_pg_version":  "PostgreSQL :     {{.Version}}",
	"restore.preview_source":      "Sauvegarde :     {{.BackupID}} ({{.Type}})",
	"restore.preview_rto":         "RTO estimé :     ~{{.Minutes}} minutes",
	"restore.preview_run_command": "Exécutez avec --confirm.",
	"restore.completed":           "restauration terminée en {{.Duration}}",

	"status.no_backups":          "aucune sauvegarde",
	"status.next_backup_at":      "prochaine sauvegarde à {{.Time}}",
	"status.next_drill_at":       "prochain exercice à {{.Time}}",
	"status.replication_active":  "active",
	"status.replication_lagging": "en retard ({{.Seconds}}s)",
	"status.replication_stalled": "bloquée",

	"err.repo_required":    "--repo est requis",
	"err.target_required":  "--target est requis",
	"err.invalid_lsn":      "LSN invalide : {{.Value}}",
	"err.invalid_time":     "date invalide : {{.Value}}",
	"err.invalid_severity": "sévérité invalide : {{.Value}}",
	"err.unknown_format":   "format de sortie inconnu : {{.Value}}",
}

var frenchPlurals = map[string]map[Form]string{
	// French: 0 + 1 are singular, 2+ are plural — but our binary
	// distinction maps cleanly: Tn(key, 1, ...) → FormOne,
	// otherwise FormOther.  0 backups uses FormOther which
	// matches the French convention "0 sauvegardes".
	"backups.count": {
		FormOne:   "{{.N}} sauvegarde",
		FormOther: "{{.N}} sauvegardes",
	},
	"deployments.count": {
		FormOne:   "{{.N}} déploiement",
		FormOther: "{{.N}} déploiements",
	},
	"chunks.count": {
		FormOne:   "{{.N}} morceau",
		FormOther: "{{.N}} morceaux",
	},
	"manifests.affected": {
		FormOne:   "{{.N}} manifeste affecté",
		FormOther: "{{.N}} manifestes affectés",
	},
}

// japaneseMessages — Japanese.  The language has no
// grammatical singular/plural distinction so the FormOne /
// FormOther collapse is natural.  Tone: polite-formal でしょう
// avoided; technical-impersonal as the CLI norm.
var japaneseMessages = map[string]string{
	"doctor.healthy":            "正常",
	"doctor.attention_required": "対応が必要",
	"doctor.last_backup_ago":    "最終バックアップ: {{.Ago}}前",
	"doctor.wal_lag":            "WAL遅延 {{.Seconds}}秒",
	"doctor.repo_unreachable":   "リポジトリに接続できません: {{.URL}}",
	"doctor.kms_unreachable":    "KMS鍵に接続できません",
	"doctor.suggested_fix":      "推奨される対応: {{.Command}}",

	"restore.refuses_live_pg":     "稼働中のPostgreSQLデータディレクトリへのリストアを拒否",
	"restore.refuses_non_empty":   "ターゲットディレクトリが空ではありません。上書きするには --force を指定",
	"restore.refuses_primary":     "現在のPatroniプライマリへのリストアを拒否",
	"restore.preview_header":      "プレビュー — 以下の内容でリストアします:",
	"restore.preview_target":      "ターゲット:    {{.TargetDir}}",
	"restore.preview_pg_version":  "PostgreSQL:    {{.Version}}",
	"restore.preview_source":      "ソース:        {{.BackupID}} ({{.Type}})",
	"restore.preview_rto":         "推定RTO:       約{{.Minutes}}分",
	"restore.preview_run_command": "実行するには --confirm を付けてください。",
	"restore.completed":           "リストアが {{.Duration}} で完了しました",

	"status.no_backups":          "バックアップがありません",
	"status.next_backup_at":      "次回バックアップ: {{.Time}}",
	"status.next_drill_at":       "次回ドリル: {{.Time}}",
	"status.replication_active":  "有効",
	"status.replication_lagging": "遅延中 ({{.Seconds}}秒)",
	"status.replication_stalled": "停止",

	"err.repo_required":    "--repo は必須です",
	"err.target_required":  "--target は必須です",
	"err.invalid_lsn":      "無効なLSN: {{.Value}}",
	"err.invalid_time":     "無効な時刻: {{.Value}}",
	"err.invalid_severity": "無効な重大度: {{.Value}}",
	"err.unknown_format":   "不明な出力形式: {{.Value}}",
}

var japanesePlurals = map[string]map[Form]string{
	// Japanese has no grammatical plural marker; FormOne and
	// FormOther render identically.  We provide both forms
	// anyway so the API surface is consistent.
	"backups.count": {
		FormOne:   "バックアップ{{.N}}件",
		FormOther: "バックアップ{{.N}}件",
	},
	"deployments.count": {
		FormOne:   "デプロイメント{{.N}}件",
		FormOther: "デプロイメント{{.N}}件",
	},
	"chunks.count": {
		FormOne:   "チャンク{{.N}}個",
		FormOther: "チャンク{{.N}}個",
	},
	"manifests.affected": {
		FormOne:   "{{.N}}件のマニフェストに影響",
		FormOther: "{{.N}}件のマニフェストに影響",
	},
}
