// llm.go — CLI surface for the LLM helper (doctor, ask, skill list/show/lint).
package cli

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/chat"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/tools"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/paths"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// newRealLlmCmd implements `pg_hardstorage llm <ask|explain|skill ...>`.
//
// v0.1 ships the read-only chat surface plus the skill management
// commands that operators need to inspect the in-tree skills.
//
// What v0.1 does NOT ship (deferred to/):
//   - Interactive TUI chat (`pg_hardstorage llm` with no subcommand).
//   - MCP server mode (--mcp-server).
//   - Web UI (--web).
//   - On-error auto-launch (--on-error).
//   - Persistent conversation history.
//   - advise+execute mode with execute_command.
//
// What it DOES ship:
//   - `llm ask`     — one-shot Q&A; no tool calls.
//   - `llm explain` — plain-English CLI explanation.
//   - `llm skill list/show/lint` — skill management.
//   - Mock + Ollama providers (in-tree).
//
// Provider selection: --provider flag wins, then $PG_HARDSTORAGE_LLM_PROVIDER
// env var, then `mock`. The mock provider is the default so the
// command works without configuration; an operator wiring a real
// provider passes --provider ollama (or sets the env var).
func newRealLlmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "llm [chat|ask|explain|skill]",
		Short: "LLM helper — grounded assistant with skill files, mandatory preview, full audit",
		Long: `LLM helper.  Drops into an interactive chat session when run
with no subcommand; one-shot Q&A via 'llm ask'; CLI-explanation
via 'llm explain'; skill management via 'llm skill ...'.

Configuration — env vars override the YAML, flags override env:

  endpoint:
    --endpoint <url>
    PG_HARDSTORAGE_URL          (canonical, project-branded)
    OPENAI_BASE_URL             (cross-tool standard, accepted)
    llm.endpoint in pg_hardstorage.yaml

  api key:
    PG_HARDSTORAGE_LLM_KEY      (canonical, project-branded)
    OPENAI_API_KEY              (cross-tool standard, accepted)
    PG_HARDSTORAGE_LLM_API_KEY  (legacy spelling, still honoured)
    llm.api_key_file in pg_hardstorage.yaml (re-read on every
                                              call — rotation
                                              survives without
                                              restart)
    llm.api_key in pg_hardstorage.yaml (inline; discouraged)

  model:
    --model <id>
    PG_HARDSTORAGE_LLM_MODEL    (canonical, project-branded)
    OPENAI_MODEL                (cross-tool standard, accepted)
    llm.model in pg_hardstorage.yaml
    provider default (gpt-4o-mini for openai)

  When pointing PG_HARDSTORAGE_URL at a non-OpenAI endpoint
  (Anthropic, Azure, Ollama, vLLM, ...) you MUST set the model
  too — gpt-4o-mini won't exist there.  A 404 from the upstream
  is returned with an actionable hint naming the env var.

When NO key resolves anywhere, the CLI falls back to the 'mock'
provider — replies are stub echoes, not real AI.  The chat
banner is loud about this so it isn't silent.

Point 'endpoint:' at any OpenAI-compatible service:
  - api.openai.com (default)
  - https://api.anthropic.com/v1 (Anthropic via OpenAI-compat)
  - http://127.0.0.1:11434/v1    (local Ollama)
  - Azure OpenAI / vLLM / OpenRouter / LiteLLM, ...

See share/pg_hardstorage.sample.yaml for a fully annotated
config.`,
		// When invoked with no subcommand, drop into chat mode —
		// the natural "I want to talk to the assistant" UX.
		// `--mcp-server` short-circuits into the MCP stdio
		// server instead (for Claude Desktop / Cursor / Zed
		// integration).
		RunE: func(cmd *cobra.Command, args []string) error {
			if mcpFlag, _ := cmd.Flags().GetBool("mcp-server"); mcpFlag {
				return runMCPServer(cmd)
			}
			return runLlmChat(cmd, llmChatOptions{
				skill:    cmd.Flag("skill").Value.String(),
				provider: cmd.Flag("provider").Value.String(),
				endpoint: cmd.Flag("endpoint").Value.String(),
				model:    cmd.Flag("model").Value.String(),
			})
		},
	}
	// Persistent flags so both `llm` (no-subcommand chat) and
	// `llm chat` accept --provider / --endpoint / --model
	// without duplicating the registration.  ask/explain still
	// have their own flag set (they don't need --skill).
	c.PersistentFlags().String("skill", "ask", "skill to use (ask, explain, restore, incident)")
	c.PersistentFlags().String("provider", "", "LLM provider override (default: env / config / 'openai' if key in scope, else 'mock')")
	c.PersistentFlags().String("endpoint", "", "provider endpoint override")
	c.PersistentFlags().String("model", "", "model id override")
	c.Flags().Bool("mcp-server", false,
		"run as a Model Context Protocol stdio server (for Claude Desktop / Cursor / Zed / Goose / Cline integration)")

	c.AddCommand(
		newLlmAskCmd(),
		newLlmExplainCmd(),
		newLlmChatCmd(),
		newLlmSkillCmd(),
		newLlmExportSessionCmd(),
		newLlmHistoryCmd(),
		newLlmDoctorCmd(),
	)
	return c
}

// --- doctor -----------------------------------------------------------

func newLlmDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Health check for the LLM helper subsystem",
		Long: `doctor verifies the LLM helper's plumbing end-to-end:

  - provider config resolves (api key + endpoint + model)
  - cheatsheet drift guard passes against the live cobra tree
  - validator catches a planted invalid command
  - hot-command paths all resolve in the catalog
  - a known-good probe round-trips to the provider

Each check returns pass/fail with a one-line summary.  Exits
non-zero when any check fails.  Read-only and cheap; safe to
run in CI.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLlmDoctor(cmd)
		},
	}
}

// llmDoctorCheck is one row in the report — operator-readable
// status + structured fields the JSON renderer surfaces verbatim.
type llmDoctorCheck struct {
	Name    string `json:"name"`
	Pass    bool   `json:"pass"`
	Detail  string `json:"detail"`
	Latency string `json:"latency,omitempty"`
}

type llmDoctorBody struct {
	Checks   []llmDoctorCheck `json:"checks"`
	Provider string           `json:"provider"`
	Endpoint string           `json:"endpoint"`
	Model    string           `json:"model"`
	OK       bool             `json:"ok"`
}

// WriteText renders the doctor checks and overall verdict as human-readable
// text to w.
func (b llmDoctorBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "[LLM helper doctor]")
	fmt.Fprintf(bw, "provider: %s · endpoint: %s · model: %s\n\n", b.Provider, b.Endpoint, b.Model)
	for _, c := range b.Checks {
		icon := "✓"
		if !c.Pass {
			icon = "✗"
		}
		lat := ""
		if c.Latency != "" {
			lat = " (" + c.Latency + ")"
		}
		fmt.Fprintf(bw, "  %s %s%s\n      %s\n", icon, c.Name, lat, c.Detail)
	}
	fmt.Fprintln(bw)
	if b.OK {
		fmt.Fprintln(bw, "OK — every check passed.")
	} else {
		fmt.Fprintln(bw, "FAILED — see ✗ rows above.")
	}
	_, err := w.Write([]byte(bw.String()))
	return err
}

func runLlmDoctor(cmd *cobra.Command) error {
	d := DispatcherFrom(cmd)
	body := llmDoctorBody{}

	// Check 1: cheatsheet drift.  No cluster contact, fast.
	checkStart := time.Now()
	driftDetail := cheatsheetDriftDetail(cmd.Root())
	body.Checks = append(body.Checks, llmDoctorCheck{
		Name:    "cheatsheet drift",
		Pass:    driftDetail == "",
		Detail:  ifEmpty(driftDetail, "no stale flag claims against the live cobra tree"),
		Latency: time.Since(checkStart).Round(time.Millisecond).String(),
	})

	// Check 2: hot-command paths all resolve.
	checkStart = time.Now()
	tree := cmdtree.Walk(cmd.Root())
	missing := []string{}
	for _, p := range hotCommandPaths {
		if tree.Find(p) == nil {
			missing = append(missing, strings.Join(p, " "))
		}
	}
	body.Checks = append(body.Checks, llmDoctorCheck{
		Name: "hot-command preload paths",
		Pass: len(missing) == 0,
		Detail: func() string {
			if len(missing) == 0 {
				return fmt.Sprintf("all %d preload paths resolve in cobra", len(hotCommandPaths))
			}
			return fmt.Sprintf("unresolved: %v", missing)
		}(),
		Latency: time.Since(checkStart).Round(time.Millisecond).String(),
	})

	// Check 3: validator catches a planted invalid command.
	checkStart = time.Now()
	planted := "pg_hardstorage status --definitely-not-a-real-flag"
	verr := cmdtree.Validate(tree, planted, "pg_hardstorage")
	body.Checks = append(body.Checks, llmDoctorCheck{
		Name:    "validator catches planted error",
		Pass:    verr != nil,
		Detail:  ifEmpty(fmt.Sprintf("validator said: %v", verr), "validator did NOT catch the planted flag — broken"),
		Latency: time.Since(checkStart).Round(time.Millisecond).String(),
	})

	// Check 4: provider resolves (api-key + endpoint + model).
	// We open the provider but don't make a real call here —
	// the round-trip check below handles that.
	checkStart = time.Now()
	prov, provName, endpoint, model, perr := resolveLlmProviderFull(cmd.Context(), "", "", "")
	body.Provider = provName
	body.Endpoint = endpoint
	body.Model = model
	body.Checks = append(body.Checks, llmDoctorCheck{
		Name: "provider configuration",
		Pass: perr == nil,
		Detail: func() string {
			if perr != nil {
				return perr.Error()
			}
			return fmt.Sprintf("%s @ %s (model=%s) opened cleanly", provName, endpoint, model)
		}(),
		Latency: time.Since(checkStart).Round(time.Millisecond).String(),
	})
	if perr != nil {
		body.OK = false
		return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
	}
	defer prov.Close()

	// Check 5: round-trip a known-good probe.  Kept tiny to bound
	// cost / latency.  We ask the model to echo a literal token
	// and check the response contains it.
	checkStart = time.Now()
	body.Checks = append(body.Checks, roundTripProbe(cmd, time.Since(checkStart)))

	// Aggregate.
	body.OK = true
	for _, c := range body.Checks {
		if !c.Pass {
			body.OK = false
			break
		}
	}
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// roundTripProbe shells back into ourselves via `llm ask` to
// confirm the provider responds.  We can't reuse the open
// provider directly because the chat session does too much
// (skill loading, tool registry, etc.); a child `llm ask`
// exercises the full path the operator uses.
func roundTripProbe(cmd *cobra.Command, _ time.Duration) llmDoctorCheck {
	checkStart := time.Now()
	// Use our own binary path — should always be argv[0]'s
	// resolved form.  Falls back to PATH lookup if we can't
	// determine our absolute path.
	bin, err := os.Executable()
	if err != nil || bin == "" {
		bin = "pg_hardstorage"
	}
	probe := "Reply with exactly one word: 'pong' (no preamble, no quotes, no markdown)."
	c := exec.CommandContext(cmd.Context(), bin, "llm", "ask", probe, "-o", "json")
	c.Env = append(append([]string{}, os.Environ()...), "PG_HARDSTORAGE_LLM_TEMPERATURE=0")
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &bytes.Buffer{}
	if err := c.Run(); err != nil {
		return llmDoctorCheck{
			Name:    "provider round-trip",
			Pass:    false,
			Detail:  fmt.Sprintf("probe call failed: %v", err),
			Latency: time.Since(checkStart).Round(time.Millisecond).String(),
		}
	}
	var d struct {
		Result struct {
			Answer string `json:"answer"`
		} `json:"result"`
	}
	_ = stdjson.Unmarshal(stdout.Bytes(), &d)
	ans := strings.ToLower(strings.TrimSpace(d.Result.Answer))
	ok := strings.Contains(ans, "pong")
	return llmDoctorCheck{
		Name: "provider round-trip",
		Pass: ok,
		Detail: func() string {
			if ok {
				return "probe returned 'pong'"
			}
			return "probe did not return 'pong'; got: " + truncatePromptForDoctor(ans, 80)
		}(),
		Latency: time.Since(checkStart).Round(time.Millisecond).String(),
	}
}

// cheatsheetDriftDetail re-runs the drift-guard logic at runtime
// and returns the first drift it finds, or "" when clean.  Mirrors
// the test helper but lives in production code so `llm doctor`
// can surface drift to the operator without depending on the
// test binary.
func cheatsheetDriftDetail(root *cobra.Command) string {
	tree := cmdtree.Walk(root)
	if tree == nil {
		return "could not walk cobra tree"
	}
	text := chat.FlagCheatsheet()
	bulletRE := regexp.MustCompile("(?m)^- `([^`]+)`(?:\\s*/\\s*`([^`]+)`)?\\s+— ")
	negRE := regexp.MustCompile("(?i)\\b(?:no|not)\\s+`--([a-z][a-z0-9-]+)`")
	matches := bulletRE.FindAllStringSubmatchIndex(text, -1)
	for i, m := range matches {
		paths := []string{text[m[2]:m[3]]}
		if m[4] != -1 {
			paths = append(paths, text[m[4]:m[5]])
		}
		start := m[1]
		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		body := text[start:end]
		for _, neg := range negRE.FindAllStringSubmatch(body, -1) {
			flagName := neg[1]
			for _, path := range paths {
				node := tree.Find(strings.Fields(path))
				if node == nil {
					return fmt.Sprintf("cheatsheet references command %q which is not in the cobra tree", path)
				}
				for _, f := range node.Flags {
					if f.Name == flagName {
						return fmt.Sprintf("cheatsheet for %q says --%s does NOT exist, but cobra has it",
							path, flagName)
					}
				}
			}
		}
	}
	return ""
}

func ifEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func truncatePromptForDoctor(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- ask --------------------------------------------------------------

func newLlmAskCmd() *cobra.Command {
	var (
		provider string
		endpoint string
		model    string
	)
	c := &cobra.Command{
		Use:   "ask <question>",
		Short: "One-shot question answering",
		Long: `Send the question to the configured LLM provider and print the
answer. Cheap; the 'ask' skill doesn't allow tool calls so no
cluster state crosses the LLM provider boundary.

The default provider is 'mock'; for real answers pass --provider
ollama (local) or set PG_HARDSTORAGE_LLM_PROVIDER.`,
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			question := strings.Join(args, " ")
			return runLlmAsk(cmd, llmAskOptions{
				skill:    "ask",
				prompt:   question,
				provider: provider,
				endpoint: endpoint,
				model:    model,
			})
		},
	}
	c.Flags().StringVar(&provider, "provider", "", "LLM provider (default: env $PG_HARDSTORAGE_LLM_PROVIDER then 'mock')")
	c.Flags().StringVar(&endpoint, "endpoint", "", "provider endpoint override (e.g. http://127.0.0.1:11434 for Ollama)")
	c.Flags().StringVar(&model, "model", "", "model id (provider-specific)")
	return c
}

// --- explain ----------------------------------------------------------

func newLlmExplainCmd() *cobra.Command {
	var (
		provider string
		endpoint string
		model    string
	)
	c := &cobra.Command{
		Use:          "explain <command-line>",
		Short:        "Explain a CLI invocation in plain English",
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmdline := strings.Join(args, " ")
			return runLlmAsk(cmd, llmAskOptions{
				skill:    "explain",
				prompt:   cmdline,
				provider: provider,
				endpoint: endpoint,
				model:    model,
			})
		},
	}
	c.Flags().StringVar(&provider, "provider", "", "LLM provider")
	c.Flags().StringVar(&endpoint, "endpoint", "", "provider endpoint")
	c.Flags().StringVar(&model, "model", "", "model id")
	return c
}

type llmAskOptions struct {
	skill    string
	prompt   string
	provider string
	endpoint string
	model    string
}

func runLlmAsk(cmd *cobra.Command, opts llmAskOptions) error {
	d := DispatcherFrom(cmd)

	// 1. Load skills (builtins + on-disk overrides).
	set, err := loadSkillSet()
	if err != nil {
		return output.NewError("llm.skill_load_failed",
			fmt.Sprintf("llm: load skills: %v", err)).Wrap(err)
	}
	skill, err := set.Get(opts.skill)
	if err != nil {
		return output.NewError("notfound.skill",
			fmt.Sprintf("llm: skill %q not loaded — drop a YAML at /etc/pg_hardstorage/skills/ or upgrade the binary (the four builtins ask/explain/restore/incident ship with+)", opts.skill)).Wrap(err)
	}

	// 2. Resolve provider.
	prov, provName, err := resolveLlmProvider(cmd.Context(), opts.provider, opts.endpoint, opts.model)
	if err != nil {
		return err
	}
	defer prov.Close()

	// 3. Build the tools registry.  For ask/restore/incident the
	// skill exposes a meaningful set of read-only tools; for
	// explain it's just docs.  RegisterCoreTools wires the
	// CLI-runner-backed live-state tools when this binary can
	// resolve its own path.
	toolReg, runner, _ := buildLiveToolRegistry(nil, cmd.Root())

	// 4. Run the session.  Bootstrap pre-loads doctor / status /
	// runbook index + the cobra command catalog into the system
	// prompt so the assistant starts grounded — the catalog is
	// the load-bearing piece for accurate command suggestions
	// (see internal/cli/cmdtree).
	tree := cmdtree.Walk(cmd.Root())
	session := &chat.Session{
		Provider:         prov,
		Tools:            toolReg,
		Skill:            skill,
		Runner:           runner,
		CommandCatalog:   cmdtree.Catalog(tree, 2),
		CommandHelpBlock: renderHotCommandHelp(tree),
		CommandValidator: func(command string) error {
			return cmdtree.Validate(tree, command, "pg_hardstorage")
		},
		// One retry round when validator catches flag-invention.
		// Doubles worst-case latency but pushes effective accuracy
		// closer to 100% for cases where the model can self-correct.
		MaxValidatorRetries: 1,
	}
	startedAt := time.Now().UTC()
	reply, err := session.Ask(cmd.Context(), opts.prompt)
	stoppedAt := time.Now().UTC()
	if err != nil {
		return output.NewError("llm.chat_failed",
			fmt.Sprintf("llm: chat: %v", err)).Wrap(err)
	}

	body := llmAskBody{
		Skill:        skill.Name,
		SkillVersion: skill.Version,
		Provider:     provName,
		Question:     opts.prompt,
		Answer:       reply.Text,
		StartedAt:    startedAt,
		StoppedAt:    stoppedAt,
		DurationMS:   stoppedAt.Sub(startedAt).Milliseconds(),
		Disclaimer:   "AI assistant — every suggestion must be verified by you before running.",
		Usage: &llmprovider.Usage{
			PromptTokens:     reply.Usage.PromptTokens,
			CompletionTokens: reply.Usage.CompletionTokens,
			TotalTokens:      reply.Usage.TotalTokens,
		},
	}
	for _, inv := range reply.ToolCalls {
		body.ToolCalls = append(body.ToolCalls, llmToolCallBody{
			Name:  inv.Name,
			Args:  inv.Args,
			Error: inv.Error,
		})
	}
	body.CommandWarnings = reply.CommandWarnings
	return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
}

// resolveLlmProvider picks + opens the LLM provider.
//
// Resolution chain (later wins for each field):
//
//  1. config file `llm:` block (pg_hardstorage.yaml + conf.d).
//  2. $PG_HARDSTORAGE_LLM_PROVIDER / $PG_HARDSTORAGE_URL /
//     $OPENAI_BASE_URL / $OPENAI_MODEL /
//     $PG_HARDSTORAGE_LLM_KEY / $OPENAI_API_KEY /
//     $PG_HARDSTORAGE_LLM_API_KEY env vars.
//  3. CLI flags (--provider, --endpoint, --model).
//
// API-key special case: file path (config) takes precedence over
// inline value (config), and either is overridden by an env-var
// key if set.  Operators wiring through systemd typically use
// `EnvironmentFile=` to load PG_HARDSTORAGE_LLM_KEY without
// baking it into the YAML.
//
// When NO provider is named AND NO API key is in scope anywhere,
// the resolver REFUSES with `llm.no_provider_configured` rather
// than silently falling through to the mock provider.  Pre-fix
// (v0.1..v0.10) the resolver dropped the operator into a mock
// REPL that echoed prompts as `mock-reply: ...`, which looked
// like a working assistant — an operator could spend minutes
// typing real questions before realising the answers were stubs.
// The mock provider remains accessible via explicit
// `--provider mock` for tests / demos / plumbing exercises.

// hotCommandPaths is the hand-picked set of subcommands whose
// FULL --help text gets baked into the system prompt at session
// bootstrap.  Evidence: each entry corresponds to a flag-invention
// failure mode observed in the operator-quality pilot (see
// the L2 *_flag_accuracy
// scenarios).  Keep this list tight — every entry costs ~200-400
// tokens in every chat session.
var hotCommandPaths = [][]string{
	// Recovery / repair surface — every entry here is a real
	// pilot or stretch failure mode.
	{"repair", "scrub"},
	{"repair", "chunks"},
	{"recovery", "drill"},
	{"recovery", "readiness"},
	{"recovery", "windows"},
	{"timetravel", "create"},
	{"restore"},

	// Day-to-day operator surface.
	{"backup"},
	{"rotate"},
	{"schedule"},
	{"doctor"},
	{"init"},
	{"deployment", "add"},

	// WAL ops.
	{"wal", "stream"},
	{"wal", "gaps"},
	{"wal", "prune"},
	{"wal", "repair"},

	// Repo ops.
	{"repo", "audit"},
	{"repo", "init"},

	// Security / encryption surface.
	{"kms", "inspect"},
	{"kms", "rotate"},
	{"kms", "shred"},
	{"threshold", "attest", "sign"},
	{"threshold", "attest", "verify"},

	// Governance / audit.
	{"audit", "search"},
	{"audit", "verify-chain"},
	{"audit", "export-bundle"},
	{"compliance", "report"},
	{"dsa", "locate"},
	{"residency", "set"},
	{"slo", "report"},

	// Capacity / cost.
	{"forecast"},
	{"capacity", "preflight"},

	// Misc high-traffic.
	{"fleet", "search"},
	{"partial", "inspect"},
	{"partial", "restore"},
	{"verify"},
	{"show"},
}

// renderHotCommandHelp produces a single rendered block holding
// the --help output for every command in hotCommandPaths.  Missing
// commands are silently skipped (covers test fixtures and partial
// command trees).
func renderHotCommandHelp(tree *cmdtree.Node) string {
	if tree == nil {
		return ""
	}
	var b strings.Builder
	for _, path := range hotCommandPaths {
		help := cmdtree.Help(tree, path)
		if help == "" {
			continue
		}
		b.WriteString(help)
		b.WriteString("\n")
	}
	return b.String()
}

// resolveLlmProviderFull is the four-return-value variant
// of resolveLlmProvider: same behaviour, but also returns
// the resolved endpoint + model so the chat banner can
// show them.  Existing callers stay on the three-value
// shim below.
func resolveLlmProviderFull(ctx context.Context, flagProvider, flagEndpoint, flagModel string) (llmprovider.Provider, string, string, string, error) {
	llmCfg := loadLLMConfigFromFile()

	// Provider name: flag → env → config-file → "openai" (when key
	// is available somewhere).  No silent mock fallback — see the
	// refusal block below.
	provName := firstNonEmpty(flagProvider,
		os.Getenv("PG_HARDSTORAGE_LLM_PROVIDER"),
		llmCfg.Provider)
	if provName == "" {
		if hasOpenAIKey(llmCfg) {
			provName = "openai"
		} else {
			return nil, "", "", "", output.NewError("llm.no_provider_configured",
				"llm: no provider configured").
				WithSuggestion(&output.Suggestion{
					Human: `set PG_HARDSTORAGE_LLM_KEY for OpenAI:
    export PG_HARDSTORAGE_LLM_KEY=sk-proj-...
    # PG_HARDSTORAGE_LLM_MODEL defaults to gpt-4o-mini; override
    # if your account uses a different model.

  Or point at a local model:
    export PG_HARDSTORAGE_URL=http://127.0.0.1:11434/v1
    export PG_HARDSTORAGE_LLM_KEY=ollama
    export PG_HARDSTORAGE_LLM_MODEL=llama3.1:8b

  Or persist in ~/.config/pg_hardstorage/pg_hardstorage.yaml — see share/pg_hardstorage.sample.yaml.

  For tests / demos: pg_hardstorage llm --provider mock`,
				}).
				Wrap(output.ErrUsage)
		}
	}
	prov, err := llmprovider.DefaultRegistry.Get(provName)
	if err != nil {
		return nil, provName, "", "", output.NewError("notfound.llm_provider",
			fmt.Sprintf("llm: provider %q not registered (registered: %v)",
				provName, llmprovider.DefaultRegistry.Names())).Wrap(err)
	}

	// Endpoint precedence: --endpoint flag → PG_HARDSTORAGE_URL
	// (canonical project-branded name) → OPENAI_BASE_URL
	// (cross-tool standard, honoured for ergonomics) →
	// llm.endpoint from yaml → provider's hardcoded default.
	//
	// Model precedence: --model flag → PG_HARDSTORAGE_LLM_MODEL
	// (canonical project-branded) → OPENAI_MODEL (cross-tool
	// standard) → llm.model from yaml → provider's default
	// (gpt-4o-mini for openai).  The default is OpenAI-specific;
	// operators pointing PG_HARDSTORAGE_URL at Anthropic / Azure
	// / Ollama / vLLM MUST set the model via env or flag, since
	// gpt-4o-mini won't exist on those endpoints.
	cfg := llmprovider.ProviderConfig{
		Endpoint: firstNonEmpty(flagEndpoint,
			os.Getenv("PG_HARDSTORAGE_URL"),
			os.Getenv("OPENAI_BASE_URL"),
			llmCfg.Endpoint),
		Model: firstNonEmpty(flagModel,
			os.Getenv("PG_HARDSTORAGE_LLM_MODEL"),
			os.Getenv("OPENAI_MODEL"),
			llmCfg.Model),
	}
	if provName == "openai" {
		key, err := resolveLLMAPIKey(llmCfg)
		if err != nil {
			return nil, provName, "", "", output.NewError("llm.api_key_unreadable",
				fmt.Sprintf("llm: %v", err)).Wrap(err)
		}
		cfg.APIKey = key
		if extra := llmCfg.Extra; len(extra) > 0 {
			cfg.Extra = extra
		}
		if llmCfg.MaxTokens > 0 {
			if cfg.Extra == nil {
				cfg.Extra = map[string]any{}
			}
			cfg.Extra["max_tokens"] = llmCfg.MaxTokens
		}
		// Temperature: env var wins over config file extra so the
		// testkit / CI can pin determinism without touching the
		// operator's yaml.  Empty string → leave unset (server default).
		if envT := os.Getenv("PG_HARDSTORAGE_LLM_TEMPERATURE"); envT != "" {
			if t, err := strconv.ParseFloat(envT, 64); err == nil {
				if cfg.Extra == nil {
					cfg.Extra = map[string]any{}
				}
				cfg.Extra["temperature"] = t
			}
		}
	}
	if err := prov.Open(ctx, cfg); err != nil {
		return nil, provName, cfg.Endpoint, cfg.Model, output.NewError("llm.provider_open_failed",
			fmt.Sprintf("llm: open %s: %v", provName, err)).Wrap(err)
	}
	return prov, provName, cfg.Endpoint, cfg.Model, nil
}

// resolveLlmProvider is the back-compat shim for callers
// that don't need the resolved endpoint + model — drops the
// last two return values from resolveLlmProviderFull.
func resolveLlmProvider(ctx context.Context, flagProvider, flagEndpoint, flagModel string) (llmprovider.Provider, string, error) {
	prov, name, _, _, err := resolveLlmProviderFull(ctx, flagProvider, flagEndpoint, flagModel)
	return prov, name, err
}

// loadLLMConfigFromFile reads pg_hardstorage.yaml + drop-ins and
// returns the merged llm: block.  Failures degrade to a zero
// LLMConfig — the helper still works via env vars / flags when
// the config file is absent or malformed.  We don't surface a
// load error here because the helper isn't the only command
// running; the doctor + status commands already report config
// failures.
func loadLLMConfigFromFile() config.LLMConfig {
	p, err := paths.Resolve(paths.DefaultOptions())
	if err != nil {
		return config.LLMConfig{}
	}
	res, err := config.Load(p)
	if err != nil || res == nil {
		return config.LLMConfig{}
	}
	return res.Config.LLM
}

// resolveLLMAPIKey applies the precedence chain for the API key:
//
//  1. $PG_HARDSTORAGE_LLM_KEY     — canonical project-branded
//     name (preferred).
//  2. $OPENAI_API_KEY             — the cross-tool standard,
//     honoured for ergonomics
//     when the same key drives
//     other CLIs on the box.
//  3. $PG_HARDSTORAGE_LLM_API_KEY — legacy spelling kept for
//     pre-v0.10 deployments;
//     silently equivalent to
//     PG_HARDSTORAGE_LLM_KEY.
//  4. llm.api_key_file from the config (read at lookup time
//     so a key rotation doesn't need a process restart).
//  5. llm.api_key inline in the config (discouraged for
//     production but accepted for compatibility).
//
// Returns ("", nil) when no key is available; the caller decides
// whether that's an error (Open will refuse for the public
// OpenAI endpoint).
func resolveLLMAPIKey(cfg config.LLMConfig) (string, error) {
	if k := os.Getenv("PG_HARDSTORAGE_LLM_KEY"); k != "" {
		return k, nil
	}
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		return k, nil
	}
	if k := os.Getenv("PG_HARDSTORAGE_LLM_API_KEY"); k != "" {
		return k, nil
	}
	if cfg.APIKeyFile != "" {
		body, err := os.ReadFile(cfg.APIKeyFile)
		if err != nil {
			return "", fmt.Errorf("read api_key_file %q: %w", cfg.APIKeyFile, err)
		}
		return strings.TrimSpace(string(body)), nil
	}
	if cfg.APIKey != "" {
		return cfg.APIKey, nil
	}
	return "", nil
}

// hasOpenAIKey reports whether the resolution chain would yield
// an API key.  Used to pick a sensible default provider when
// neither --provider nor $PG_HARDSTORAGE_LLM_PROVIDER is set.
// Mirrors resolveLLMAPIKey's precedence — the new
// PG_HARDSTORAGE_LLM_KEY name is recognised alongside the
// legacy spellings.
func hasOpenAIKey(cfg config.LLMConfig) bool {
	return os.Getenv("PG_HARDSTORAGE_LLM_KEY") != "" ||
		os.Getenv("OPENAI_API_KEY") != "" ||
		os.Getenv("PG_HARDSTORAGE_LLM_API_KEY") != "" ||
		cfg.APIKey != "" || cfg.APIKeyFile != ""
}

// firstNonEmpty returns the first argument that is not empty.
// Trivial helper but reads better at the call sites than nested
// ifs over the resolution chain.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildLiveToolRegistry constructs the tool registry the chat
// session uses.  Always includes the always-safe tools
// (suggest_command, preview_command, read_runbook,
// list_runbooks, search_docs).  When the binary can resolve its
// own path, additionally registers the live-state tools backed
// by a CLIRunner.  In test environments where os.Executable()
// fails (rare), we degrade to the always-safe-only set so the
// docs-only skills (explain) keep working.
//
// In advise+execute mode (gateState != nil), registers
// execute_command bound to the safety gate stack.  The default
// path (gateState == nil) is read-only — execute_command never
// reaches the model.
func buildLiveToolRegistry(gateState *toolGateState, cmdRoot *cobra.Command) (*tools.Registry, *tools.CLIRunner, error) {
	reg := tools.NewRegistry()
	// Re-register the always-safe builtin tools that
	// internal/llm/tools/init() registered against
	// DefaultRegistry; we want a fresh registry per session so
	// tests can isolate tool sets, but we want the same
	// always-safe set as a floor.
	for _, t := range tools.DefaultRegistry.All() {
		// preview_command needs the per-session preview ledger
		// when advise+execute is active; the DefaultRegistry's
		// instance has nil PreviewState, which preserves the
		// read-only-mode contract (preview_command works but
		// doesn't record).
		reg.Register(t)
	}
	if gateState != nil {
		// Replace preview_command with one wired to the gate's
		// preview ledger so execute_command can verify replays.
		reg.Register(&tools.PreviewCommandWithLedger{Ledger: gateState.preview})
	}
	// Walk the cobra tree once per registry build and wire
	// the command-introspection tools.  The same tree backs
	// read_command_help (operator-facing lookup), the
	// suggest_command validator (Layer 3), and the
	// post-response backtick scrubber (Layer 4).  Tests
	// pass a nil cmdRoot when they exercise tools that
	// don't need it; the read_command_help tool then
	// degrades to a "tool unavailable" Result and
	// suggest_command falls back to echo-only.
	var cmdTree *cmdtree.Node
	if cmdRoot != nil {
		cmdTree = cmdtree.Walk(cmdRoot)
	}
	reg.Register(tools.ReadCommandHelp{Tree: cmdTree})
	// Replace the default echo-only suggest_command with
	// the validating instance.  Order matters — this
	// overrides the registration the DefaultRegistry copy
	// loop made above.
	reg.Register(tools.SuggestCommand{Tree: cmdTree})
	path, err := tools.ResolveSelf()
	if err != nil {
		// Resolve-self failed (rare).  The session works without
		// live-state tools — the assistant still has the docs
		// corpus and the suggest/preview tools.
		return reg, nil, err
	}
	// Skip live-state tools when running under `go test` — the
	// resolved binary is the test binary, and forking it with
	// CLI args (`pg_hardstorage doctor -o json`) hangs because
	// the test binary doesn't recognise them as test flags.
	// Production paths always have the real binary at path.
	if isTestBinary(path) {
		return reg, nil, nil
	}
	runner := &tools.CLIRunner{Path: path}
	tools.RegisterCoreTools(reg, runner)
	if gateState != nil {
		// Wire execute_command bound to the gate state.  The
		// fifth gate (anomaly-refusal) is always wired in
		// advise+execute mode; its detector reads recent topic
		// tokens from the chat orchestrator's running tally.
		reg.Register(&tools.ExecuteCommand{
			Mode:            gateState.mode,
			Policy:          gateState.policy,
			Preview:         gateState.preview,
			Anomaly:         gateState.anomaly,
			Runner:          runner,
			AuditCallback:   gateState.auditCallback,
			AnomalyCallback: gateState.anomalyCallback,
		})
	}
	return reg, runner, nil
}

// isTestBinary reports whether path looks like a Go test binary.
// Go test produces binaries with names ending in `.test` or
// containing `/cli.test` etc.; we match conservatively on both
// suffix patterns.
func isTestBinary(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".test") {
		return true
	}
	// Race-detector binaries on some platforms have a different
	// suffix.  Detect via the test-flag presence as a fallback.
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-test.") {
			return true
		}
	}
	return false
}

// toolGateState bundles the advise+execute state the chat CLI
// passes through to buildLiveToolRegistry.  Constructed at
// session start when --mode advise+execute is set.
type toolGateState struct {
	mode            safety.ExecMode
	policy          safety.SkillExecPolicy
	preview         *safety.PreviewState
	anomaly         *safety.AnomalyDetector
	auditCallback   func(safety.GateDecision, string)
	anomalyCallback func(safety.AnomalyDecision, string)
}

// --- skill ------------------------------------------------------------

func newLlmSkillCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "skill <list|show|lint|install|rollback|history>",
		Short: "Manage LLM skill files",
	}
	c.AddCommand(
		newLlmSkillListCmd(),
		newLlmSkillShowCmd(),
		newLlmSkillLintCmd(),
		newLlmSkillInstallCmd(),
		newLlmSkillRollbackCmd(),
		newLlmSkillHistoryCmd(),
	)
	return c
}

func newLlmSkillListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "List loaded skills (precedence chain merged)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := DispatcherFrom(cmd)
			set, err := loadSkillSet()
			if err != nil {
				return output.NewError("llm.skill_load_failed",
					fmt.Sprintf("llm skill list: %v", err)).Wrap(err)
			}
			body := skillListBody{Skills: nil}
			for _, s := range set.All() {
				body.Skills = append(body.Skills, skillListEntry{
					Name:        s.Name,
					DisplayName: s.DisplayName,
					Version:     s.Version,
					Description: firstLine(s.Description),
					Source:      s.Source,
					ReadOnly:    s.Permissions.ReadOnly,
				})
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		},
	}
}

func newLlmSkillShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "show <name>",
		Short:        "Show one loaded skill",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			set, err := loadSkillSet()
			if err != nil {
				return output.NewError("llm.skill_load_failed",
					fmt.Sprintf("llm skill show: %v", err)).Wrap(err)
			}
			s, err := set.Get(args[0])
			if err != nil {
				return output.NewError("notfound.skill",
					fmt.Sprintf("llm skill show: %q", args[0])).Wrap(err)
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(skillShowBody{Skill: s}))
		},
	}
}

func newLlmSkillLintCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "lint <path-or-name>",
		Short:        "Validate a skill YAML file (or a loaded skill name)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := DispatcherFrom(cmd)
			arg := args[0]
			var s *skills.Skill
			var err error
			if isPath(arg) {
				s, err = skills.LoadFile(arg)
			} else {
				set, err2 := loadSkillSet()
				if err2 != nil {
					return output.NewError("llm.skill_load_failed",
						fmt.Sprintf("llm skill lint: %v", err2)).Wrap(err2)
				}
				s, err = set.Get(arg)
			}
			if err != nil {
				return output.NewError("llm.skill_lint_failed",
					fmt.Sprintf("llm skill lint: %v", err)).Wrap(err)
			}
			body := skillLintBody{
				Name:    s.Name,
				Version: s.Version,
				Source:  s.Source,
				Issues:  s.Lint(),
			}
			return d.Result(output.NewResult(cmd.CommandPath()).WithBody(body))
		},
	}
}

// --- helpers ----------------------------------------------------------

func loadSkillSet() (*skills.Set, error) {
	dirs := skills.DefaultDirs(os.Getenv("HOME"))
	// Prepend an in-development fallback so a `make build` without
	// install still finds the in-tree skills.
	dirs = append([]string{"share/skills"}, dirs...)
	if extra := os.Getenv("PG_HARDSTORAGE_SKILL_DIR"); extra != "" {
		dirs = append(dirs, extra)
	}
	// LoadAllWithBuiltins layers the four bundled skills (ask,
	// explain, restore, incident) under any on-disk overrides the
	// operator has dropped at /etc/pg_hardstorage/skills/ or
	// ~/.config/pg_hardstorage/skills/.  Builtins guarantee the
	// minimum set is always present — the helper works on a fresh
	// install without external configuration.
	return skills.LoadAllWithBuiltins(dirs)
}

func isPath(s string) bool {
	return strings.Contains(s, string(filepath.Separator)) ||
		strings.HasSuffix(s, ".yaml") || strings.HasSuffix(s, ".yml")
}

func firstLine(s string) string {
	idx := strings.IndexByte(s, '\n')
	if idx < 0 {
		return s
	}
	return s[:idx]
}

// --- bodies -----------------------------------------------------------

type llmAskBody struct {
	Skill           string                `json:"skill"`
	SkillVersion    string                `json:"skill_version"`
	Provider        string                `json:"provider"`
	Question        string                `json:"question"`
	Answer          string                `json:"answer"`
	ToolCalls       []llmToolCallBody     `json:"tool_calls,omitempty"`
	CommandWarnings []chat.CommandWarning `json:"command_warnings,omitempty"`
	Usage           *llmprovider.Usage    `json:"usage,omitempty"`
	StartedAt       time.Time             `json:"started_at"`
	StoppedAt       time.Time             `json:"stopped_at"`
	DurationMS      int64                 `json:"duration_ms"`
	Disclaimer      string                `json:"disclaimer"`
}

// llmToolCallBody summarises one tool invocation.  We surface
// name + args + (optional) error; the full tool result body is
// kept in the model's context but not echoed to the user (it
// can be very large; the assistant's text answer already
// summarises it).  Operators wanting the full bodies will get
// them via `llm export-session` (chunk for).
type llmToolCallBody struct {
	Name  string         `json:"name"`
	Args  map[string]any `json:"args,omitempty"`
	Error string         `json:"error,omitempty"`
}

// WriteText renders the assistant's answer along with any tool-call and
// command-validator warnings as human-readable text to w.
func (b llmAskBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintln(bw, "[AI assistant — verify every suggestion before running]")
	fmt.Fprintf(bw, "skill: %s v%s · provider: %s\n\n", b.Skill, b.SkillVersion, b.Provider)
	fmt.Fprintln(bw, b.Answer)
	if len(b.ToolCalls) > 0 {
		fmt.Fprintf(bw, "\nTools consulted (%d):\n", len(b.ToolCalls))
		for _, tc := range b.ToolCalls {
			marker := "✓"
			if tc.Error != "" {
				marker = "✗"
			}
			fmt.Fprintf(bw, "  %s %s", marker, tc.Name)
			if tc.Error != "" {
				fmt.Fprintf(bw, " — %s", tc.Error)
			}
			fmt.Fprintln(bw)
		}
	}
	if len(b.CommandWarnings) > 0 {
		fmt.Fprintf(bw, "\nCommand-validator warnings (%d) — verify before running:\n", len(b.CommandWarnings))
		for _, w := range b.CommandWarnings {
			fmt.Fprintf(bw, "  ⚠  %s\n     → %s\n", w.Command, w.Issue)
		}
	}
	if b.Usage != nil {
		fmt.Fprintf(bw, "\n— %d prompt tokens · %d completion tokens · %d ms",
			b.Usage.PromptTokens, b.Usage.CompletionTokens, b.DurationMS)
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type skillListBody struct {
	Skills []skillListEntry `json:"skills"`
}

type skillListEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	ReadOnly    bool   `json:"read_only"`
}

// WriteText renders the available skills as a tabular summary to w.
func (b skillListBody) WriteText(w io.Writer) error {
	if len(b.Skills) == 0 {
		_, err := io.WriteString(w, "no skills loaded — install share/skills/ or drop a YAML in /etc/pg_hardstorage/skills/")
		return err
	}
	bw := &strings.Builder{}
	tw := tabwriter.NewWriter(bw, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tDESCRIPTION\tSOURCE")
	for _, s := range b.Skills {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, s.Version, s.Description, s.Source)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type skillShowBody struct {
	Skill *skills.Skill `json:"skill"`
}

// WriteText renders one skill's metadata and description as human-readable
// text to w.
func (b skillShowBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	fmt.Fprintf(bw, "skill: %s v%s\n", b.Skill.Name, b.Skill.Version)
	if b.Skill.DisplayName != "" {
		fmt.Fprintf(bw, "  display:      %s\n", b.Skill.DisplayName)
	}
	fmt.Fprintf(bw, "  source:       %s\n", b.Skill.Source)
	fmt.Fprintf(bw, "  read-only:    %v\n", b.Skill.Permissions.ReadOnly)
	if len(b.Skill.Context.AvailableTools) > 0 {
		fmt.Fprintf(bw, "  tools:        %s\n", strings.Join(b.Skill.Context.AvailableTools, ", "))
	}
	if b.Skill.Description != "" {
		fmt.Fprintf(bw, "\nDescription:\n  %s\n",
			strings.ReplaceAll(strings.TrimSpace(b.Skill.Description), "\n", "\n  "))
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

type skillLintBody struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Source  string   `json:"source,omitempty"`
	Issues  []string `json:"issues"`
}

// WriteText renders the skill-lint outcome — clean pass or per-issue list —
// as human-readable text to w.
func (b skillLintBody) WriteText(w io.Writer) error {
	bw := &strings.Builder{}
	if len(b.Issues) == 0 {
		fmt.Fprintf(bw, "✓ skill %s v%s — no lint issues", b.Name, b.Version)
	} else {
		fmt.Fprintf(bw, "skill %s v%s — %d issue(s):\n", b.Name, b.Version, len(b.Issues))
		for _, iss := range b.Issues {
			fmt.Fprintf(bw, "  - %s\n", iss)
		}
	}
	_, err := io.WriteString(w, strings.TrimRight(bw.String(), "\n"))
	return err
}

// silence unused imports during dev iterations.
var (
	_ = context.Background
	_ = tools.DefaultRegistry
)
