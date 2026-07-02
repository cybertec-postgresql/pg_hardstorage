// llm_chat.go — 'llm chat' CLI verb: interactive REPL backed by the LLM provider + skills.
package cli

import (
	"bufio"
	"context"
	stdjson "encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/cli/cmdtree"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/config/configcheck"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/chat"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/privacy"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/safety"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/skills"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/plugin/llmprovider"
)

// newLlmChatCmd implements `pg_hardstorage llm chat`.  Same as
// the no-subcommand `pg_hardstorage llm` invocation; we expose
// the explicit form so scripts that want the interactive
// behaviour aren't relying on cobra's default-RunE path.
//
// Flags (--skill, --provider, --endpoint, --model) are
// inherited from the parent `llm` command's PersistentFlags
// so both invocation shapes share the same flag set.
func newLlmChatCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "chat",
		Short: "Interactive chat session — REPL with /show-* transparency commands",
		Long: `Drop into an interactive chat session.  Each line of input is
sent to the configured LLM as a user turn; tool calls fire
automatically; the assistant's response streams back to the
terminal.

Slash commands (typed at the prompt):
  /help                show this list
  /exit, /quit         leave the session
  /clear               drop the conversation history (keep system prompt)
  /show-context        running session metadata (skill, provider, tokens)
  /show-tools          tool surface available to the active skill
  /show-skill          active skill name + version + source path
  /show-budget         tokens used + remaining

The default skill is 'ask' (general-purpose Q&A).  Switch with
` + "`" + `--skill` + "`" + ` (e.g. ` + "`" + `--skill incident` + "`" + ` for incident-response
mode).`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			auditRepo, _ := cmd.Flags().GetString("audit-repo")
			mode, _ := cmd.Flags().GetString("mode")
			noHistory, _ := cmd.Flags().GetBool("no-history")
			historyKeyFile, _ := cmd.Flags().GetString("history-key-file")
			principal, _ := cmd.Flags().GetString("principal")
			return runLlmChat(cmd, llmChatOptions{
				skill:          cmd.Flag("skill").Value.String(),
				provider:       cmd.Flag("provider").Value.String(),
				endpoint:       cmd.Flag("endpoint").Value.String(),
				model:          cmd.Flag("model").Value.String(),
				auditRepo:      auditRepo,
				execMode:       mode,
				noHistory:      noHistory,
				historyKeyFile: historyKeyFile,
				principal:      principal,
			})
		},
	}
	c.Flags().String("audit-repo", "",
		"capture every llm.* event into the named repo's audit chain (export later via `llm export-session <id> --repo <url>`)")
	c.Flags().String("mode", "read-only",
		"execution mode: read-only (default; execute_command refuses every invocation) | advise+execute (gated invocation per the safety stack)")
	c.Flags().Bool("no-history", false,
		"don't write an encrypted transcript to <state>/llm/conversations/. Default: record when a local KEK is available")
	c.Flags().String("history-key-file", "",
		"path to a 32-byte hex file overriding the local-KEK-derived history key (use when no kek.bin exists)")
	c.Flags().String("principal", "",
		"operator principal for per-principal history isolation (default: $USER, or 'anonymous')")
	return c
}

type llmChatOptions struct {
	skill    string
	provider string
	endpoint string
	model    string

	// auditRepo, when non-empty, names a repository whose audit
	// chain receives every llm.* event from this session.  Used
	// for accountability + post-incident replay; the resulting
	// bundle is exportable via `llm export-session <id> --repo
	// <url>`.
	auditRepo string

	// execMode is "read-only" (default; execute_command refuses
	// every invocation) or "advise+execute" (gated invocation per
	// the internal/llm/safety stack: replay-protection via
	// preview_command, allowed_executes prefix policy, mutation-
	// flag refusal).  Anything else fails at parse time.
	execMode string

	// initialUserMessage, when non-empty, is sent as the first
	// turn before the REPL takes over — used by --on-error-llm
	// auto-launch to seed the conversation with the failure
	// context.
	initialUserMessage string

	// noHistory disables the per-principal encrypted transcript
	// recording.  Default false (record when a key source is
	// available).  When the keyring has no kek.bin AND
	// historyKeyFile is empty, history is silently skipped.
	noHistory bool

	// historyKeyFile, when non-empty, points at a 32-byte hex
	// file the history Writer uses as its DEK source — used
	// when the operator runs cloud-KMS-only and has no local
	// KEK from which to derive.
	historyKeyFile string

	// principal scopes per-user history isolation.  Empty
	// defaults to $USER (or "anonymous").
	principal string
}

// runLlmChat is the chat REPL.  Reads stdin one line at a time,
// dispatches `/`-prefixed slash commands locally, sends
// everything else to the LLM via Session.Ask, prints the reply.
func runLlmChat(cmd *cobra.Command, opts llmChatOptions) error {
	if opts.skill == "" {
		opts.skill = "ask"
	}
	stdout := cmd.OutOrStdout()
	stdin := cmd.InOrStdin()

	// 1. Load skills.
	set, err := loadSkillSet()
	if err != nil {
		return output.NewError("llm.skill_load_failed",
			fmt.Sprintf("llm chat: load skills: %v", err)).Wrap(err)
	}
	skill, err := set.Get(opts.skill)
	if err != nil {
		return output.NewError("notfound.skill",
			fmt.Sprintf("llm chat: skill %q not loaded — try 'llm skill list'", opts.skill)).Wrap(err)
	}

	// 2. Resolve provider (same chain as `llm ask`).  We use the
	//    -Full variant so the chat banner can show the resolved
	//    endpoint + model — the two questions an operator asks
	//    first when a chat seems off ("am I talking to the local
	//    Ollama or to OpenAI? which model?").
	prov, provName, resolvedEndpoint, resolvedModel, err := resolveLlmProviderFull(cmd.Context(), opts.provider, opts.endpoint, opts.model)
	if err != nil {
		return err
	}
	defer prov.Close()

	// 3. Resolve advise+execute mode.  Default read-only;
	//    advise+execute requires the active skill to declare
	//    AllowedExecutes (otherwise the gate refuses every
	//    invocation regardless).
	execMode, gateState, err := resolveExecMode(opts.execMode, skill)
	if err != nil {
		return err
	}

	// 4. Build tool registry.  When advise+execute is active,
	//    gateState is non-nil and execute_command is registered;
	//    in read-only mode, execute_command is absent.
	toolReg, runner, _ := buildLiveToolRegistry(gateState, cmd.Root())

	// 4. Resolve privacy mode + endpoint for the session.  The
	//    config-file llm.privacy field is the source of truth;
	//    operators with no config get the standard default.
	llmCfg := loadLLMConfigFromFile()
	mode, modeErr := privacy.Parse(llmCfg.Privacy)
	if modeErr != nil {
		return output.NewError("config.bad_privacy_mode", modeErr.Error()).Wrap(modeErr)
	}
	// Use the endpoint the provider actually resolved (flag → env →
	// yaml → provider default) so the privacy gate enforces against
	// the same host requests will hit.  Resolving flag→yaml only here
	// would skip PG_HARDSTORAGE_URL / OPENAI_BASE_URL, letting
	// local-only silently egress to a public endpoint (or wrongly
	// refuse a local env endpoint).
	endpoint := resolvedEndpoint

	// 6. Build the session.  Bootstrap pre-loads cluster state
	//    into the system prompt; the operator's first prompt
	//    arrives with the cluster already grounded.
	//
	//    The command catalog is rendered from the live cobra
	//    tree so the model has ground truth about valid verbs.
	//    Without it the model improvises from CLI conventions
	//    and types `deployment create --name X` (the actual
	//    bug an operator hit before this layer landed); with
	//    it the catalog block tells the model the real shape
	//    is `deployment add <name> --connection ... --repo ...`.
	chatCmdTree := cmdtree.Walk(cmd.Root())
	cmdCatalog := cmdtree.Catalog(chatCmdTree, 2)
	session := &chat.Session{
		Provider:        prov,
		Tools:           toolReg,
		Skill:           skill,
		Runner:          runner,
		Privacy:         mode,
		PrivacyEndpoint: endpoint,
		ExecMode:        execMode,
		CommandCatalog:  cmdCatalog,
	}
	if gateState != nil {
		session.PreviewLedger = gateState.preview
	}
	// 4a. Wire the audit-chain emitter when --audit-repo points
	//     at a repo with a usable audit chain.  Failures degrade
	//     silently — chat still works without an audit trail.
	if opts.auditRepo != "" {
		if emitter, err := buildAuditEmitter(cmd.Context(), opts.auditRepo); err == nil && emitter != nil {
			session.AuditEmitter = emitter
		} else if err != nil {
			fmt.Fprintf(stdout, "  (audit emitter init failed: %v — continuing without audit)\n", err)
		}
	}
	// 4b. Wire the per-principal encrypted history Writer.
	//     The DEK is derived deterministically from the local
	//     KEK (kek.bin in the keyring) via HKDF-SHA256, so
	//     operators don't have to track a separate history
	//     key file.  When no kek.bin is present, history is
	//     skipped — the chat still runs, but no transcript is
	//     recorded.  Operators wanting history without a local
	//     KEK use --history-key-file to point at an external
	//     32-byte hex key.
	historyWriter, historyClose, err := openHistoryWriter(opts, skill, prov)
	if err != nil {
		fmt.Fprintf(stdout, "  (history disabled: %v)\n", err)
	} else if historyWriter != nil {
		session.HistoryWriter = historyWriter
		defer func() {
			// Close pulls together the encrypted body + meta
			// sidecar; flush on session exit (clean or via
			// signal-driven defer chain).
			_ = historyClose()
		}()
	}
	if err := session.Bootstrap(cmd.Context()); err != nil {
		return output.NewError("llm.bootstrap_failed",
			fmt.Sprintf("llm chat: bootstrap: %v", err)).Wrap(err)
	}

	// 5. Welcome banner + REPL loop.
	printChatBanner(stdout, skill, provName, resolvedEndpoint, resolvedModel)

	// 5a. Optional initial user message (--on-error-llm passes
	// the failure context here).  We dispatch it once before
	// dropping into the input loop so the first thing the
	// operator sees is the assistant's grounded analysis.
	if opts.initialUserMessage != "" {
		fmt.Fprintf(stdout, "> %s\n", opts.initialUserMessage)
		if err := streamChatTurn(cmd.Context(), stdout, session, opts.initialUserMessage, chatCmdTree); err != nil {
			fmt.Fprintf(stdout, "error: %v\n", err)
		}
	}

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 1<<20) // 1 MiB max line
	for {
		fmt.Fprint(stdout, "> ")
		if !scanner.Scan() {
			// Scan()==false is either a clean EOF (Ctrl-D) or a
			// read error / oversized line (bufio.ErrTooLong when a
			// single line exceeds the 1 MiB buffer).  Distinguish
			// them: a nil Err() is a graceful exit; a non-nil Err()
			// must be surfaced rather than silently ending the
			// session like Ctrl-D.
			fmt.Fprintln(stdout)
			if serr := scanner.Err(); serr != nil {
				return output.NewError("llm.chat_read_failed",
					fmt.Sprintf("llm chat: read input: %v", serr)).Wrap(serr)
			}
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			done, err := handleSlash(stdout, session, line)
			if err != nil {
				fmt.Fprintf(stdout, "error: %v\n", err)
			}
			if done {
				return nil
			}
			continue
		}
		// User turn — send to the assistant, stream the reply.
		ctx := cmd.Context()
		if err := streamChatTurn(ctx, stdout, session, line, chatCmdTree); err != nil {
			fmt.Fprintf(stdout, "error: %v\n", err)
		}
	}
}

// resolveExecMode parses the --mode flag and constructs the
// gate state advise+execute needs.  Read-only returns
// (ModeReadOnly, nil, nil) so the registry skips registering
// execute_command.  advise+execute returns the populated gate
// state, including the active skill's AllowedExecutes policy.
//
// Refuses advise+execute for skills that declare execute_command
// in available_tools but DON'T list any allowed_executes — a
// skill author must explicitly bound the blast radius.
func resolveExecMode(modeFlag string, skill *skills.Skill) (safety.ExecMode, *toolGateState, error) {
	switch strings.ToLower(strings.TrimSpace(modeFlag)) {
	case "", "read-only", "readonly":
		return safety.ModeReadOnly, nil, nil
	case "advise+execute", "advise-execute", "advise_execute":
		// fall through
	default:
		return "", nil, output.NewError("usage.bad_mode",
			fmt.Sprintf("llm chat: --mode %q unrecognised (want read-only | advise+execute)", modeFlag)).Wrap(output.ErrUsage)
	}

	// In advise+execute mode the skill must declare BOTH
	// execute_command in available_tools AND a non-empty
	// allowed_executes list.  Either alone is a configuration
	// error; we surface a clear refusal rather than silently
	// running with an empty allowlist (which would refuse
	// everything anyway, but with no signal to the operator
	// about why).
	hasExecute := false
	for _, t := range skill.Context.AvailableTools {
		if t == "execute_command" {
			hasExecute = true
			break
		}
	}
	if !hasExecute {
		return "", nil, output.NewError("usage.bad_mode",
			fmt.Sprintf("llm chat: skill %q does not declare execute_command in available_tools — advise+execute mode has nothing to execute", skill.Name)).Wrap(output.ErrUsage)
	}
	if len(skill.Context.AllowedExecutes) == 0 {
		return "", nil, output.NewError("usage.bad_mode",
			fmt.Sprintf("llm chat: skill %q declares execute_command but no allowed_executes — refuse to enable advise+execute with an empty allowlist (skill author must list permitted command prefixes)", skill.Name)).Wrap(output.ErrUsage)
	}

	state := &toolGateState{
		mode: safety.ModeAdviseExecute,
		policy: safety.SkillExecPolicy{
			AllowedExecutes: skill.Context.AllowedExecutes,
		},
		preview: &safety.PreviewState{},
		// Fifth gate: anomaly-refusal.  We start with
		// the default high-risk verb list and an empty topic
		// set; the chat orchestrator updates RecentTopicTokens
		// from each user prompt + assistant response so the
		// detector knows what's on-topic.
		anomaly: &safety.AnomalyDetector{
			HighRiskVerbs:     safety.DefaultHighRiskVerbs,
			RecentTopicTokens: map[string]struct{}{},
		},
	}
	return safety.ModeAdviseExecute, state, nil
}

// streamChatTurn drives one user turn through the session.  We
// don't have a streaming-token API at the chat.Session level
// (that lives in the provider); for now we wait for the full
// reply and print it.  A future commit can wire token-level
// streaming through Session for snappier feedback.
//
// chatTree, when non-nil, drives the post-response command
// scrubber (Layer 4) — it scans the assistant's text for
// backtick-wrapped pg_hardstorage commands and surfaces a
// warning block when any of them don't parse against the
// live cobra tree.  Scrubbing covers the case where the
// model writes a command in plain prose without going
// through suggest_command (where the tool-call gate would
// have caught it).
func streamChatTurn(ctx context.Context, w io.Writer, session *chat.Session, prompt string, chatTree *cmdtree.Node) error {
	startedAt := time.Now()
	reply, err := session.Ask(ctx, prompt)
	took := time.Since(startedAt)
	if err != nil {
		return err
	}
	// Print tool-call preamble (✓/✗) so the operator sees what
	// was consulted before the answer.
	if len(reply.ToolCalls) > 0 {
		for _, tc := range reply.ToolCalls {
			marker := "✓"
			if tc.Error != "" {
				marker = "✗"
			}
			fmt.Fprintf(w, "  %s %s", marker, tc.Name)
			if tc.Error != "" {
				fmt.Fprintf(w, " — %s", tc.Error)
			}
			fmt.Fprintln(w)
		}
	}
	if reply.Text != "" {
		fmt.Fprintln(w, reply.Text)
	} else {
		fmt.Fprintln(w, "(no text response)")
	}
	// Layer 4: post-response command scrubber.  Findings
	// whose Error is non-nil mean the model emitted a
	// command in prose that doesn't parse — show a clear
	// warning block underneath the answer with the bad
	// shape + the validator's hint, so the operator
	// doesn't try to run it.
	if chatTree != nil && reply.Text != "" {
		findings := cmdtree.Scrub(chatTree, reply.Text, "pg_hardstorage")
		renderScrubFindings(w, findings)
	}
	// Layer 4b: post-response CONFIG scrubber. The command-validator
	// only sees CLI commands; this catches invented pg_hardstorage.yaml
	// keys (e.g. a flat `backup_schedule:` instead of the nested
	// `schedule.backup.every`) the operator would otherwise paste in.
	if reply.Text != "" {
		renderConfigFindings(w, configcheck.Scrub(reply.Text))
	}
	fmt.Fprintf(w, "  — %d prompt + %d completion tokens · %dms\n\n",
		reply.Usage.PromptTokens, reply.Usage.CompletionTokens, took.Milliseconds())
	return nil
}

// renderScrubFindings prints a warning block for each
// invalid command Scrub found.  Valid commands are
// silent — no point telling the operator that 5/5 of
// the suggestions parse cleanly, that's just noise.
func renderScrubFindings(w io.Writer, findings []cmdtree.ScrubFinding) {
	bad := 0
	for _, f := range findings {
		if f.Error != nil {
			bad++
		}
	}
	if bad == 0 {
		return
	}
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "⚠ command-validation warnings (%d of %d quoted commands did not parse):\n", bad, len(findings))
	for _, f := range findings {
		if f.Error == nil {
			continue
		}
		fmt.Fprintf(w, "  · %s\n", f.Command)
		fmt.Fprintf(w, "    %s\n", f.Error.Error())
		if len(f.Error.PathBeforeError) > 0 {
			fmt.Fprintf(w, "    (run `pg_hardstorage %s --help` to see the real verb / flag list)\n",
				strings.Join(f.Error.PathBeforeError, " "))
		}
	}
	fmt.Fprintln(w, "")
}

// renderConfigFindings prints a warning block for invented pg_hardstorage.yaml
// keys the config-validator found. Silent when the YAML is clean (or absent).
func renderConfigFindings(w io.Writer, findings []configcheck.Finding) {
	if len(findings) == 0 {
		return
	}
	noun := "issue"
	if len(findings) != 1 {
		noun = "issues"
	}
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "⚠ config-validation warnings (%d %s in the suggested YAML):\n", len(findings), noun)
	for _, f := range findings {
		loc := f.Path
		if loc == "" {
			loc = "(config root)"
		}
		switch f.Kind {
		case configcheck.KindUnknownKey:
			fmt.Fprintf(w, "  · unknown key %q under %s\n", f.Key, loc)
			if f.Suggestion != "" {
				fmt.Fprintf(w, "    did you mean %q?\n", f.Suggestion)
			}
		case configcheck.KindOneOf:
			fmt.Fprintf(w, "  · %s\n", f.Message)
		default: // type / enum
			fmt.Fprintf(w, "  · key %q under %s: %s\n", f.Key, loc, f.Message)
		}
	}
	fmt.Fprintln(w, "")
}

// handleSlash dispatches a `/`-prefixed command.  Returns
// (done=true) when the loop should exit.
func handleSlash(w io.Writer, session *chat.Session, line string) (bool, error) {
	cmd := strings.TrimPrefix(line, "/")
	cmd = strings.TrimSpace(cmd)
	switch {
	case cmd == "exit", cmd == "quit", cmd == "q":
		return true, nil
	case cmd == "help", cmd == "?":
		printHelp(w)
		return false, nil
	case cmd == "clear":
		// Drop user / assistant history; keep the system message.
		if len(session.History) > 0 {
			session.History = session.History[:1]
		}
		fmt.Fprintln(w, "  conversation cleared (system prompt preserved)")
		return false, nil
	case cmd == "show-context", cmd == "context":
		snap := session.SnapshotContext()
		body, _ := stdjson.MarshalIndent(snap, "  ", "  ")
		fmt.Fprintf(w, "  %s\n", body)
		return false, nil
	case cmd == "show-tools", cmd == "tools":
		printTools(w, session)
		return false, nil
	case cmd == "show-skill", cmd == "skill":
		printSkill(w, session.Skill)
		return false, nil
	case cmd == "show-budget", cmd == "budget":
		snap := session.SnapshotContext()
		fmt.Fprintf(w, "  used: %v · budget: %v\n", snap["used_tokens"], snap["token_budget"])
		return false, nil
	default:
		return false, fmt.Errorf("unknown command %q (try /help)", cmd)
	}
}

func printChatBanner(w io.Writer, skill *skills.Skill, providerName, endpoint, model string) {
	c := newBannerStyle(w)

	fmt.Fprintln(w, c.dim("[AI assistant — verify every suggestion before running]"))
	fmt.Fprintf(w, "%s %s · %s %s · %s %s\n",
		c.label("skill:"), c.value(skill.Name+" v"+skill.Version),
		c.label("provider:"), c.value(providerName),
		c.label("model:"), c.value(displayOrUnset(model)),
	)
	// Endpoint goes on its own line — URLs are long, and an
	// operator scanning the banner cares about it as a
	// distinct fact ("am I pointed at the right host?").
	fmt.Fprintf(w, "%s %s\n", c.label("url:"), c.value(displayOrUnset(endpoint)))
	fmt.Fprintln(w, c.dim("/help for commands · /exit to quit"))
	if providerName == "mock" {
		// Reachable only via explicit `--provider mock` since
		// the silent-fallback path was removed in v0.10 — an
		// operator who lands here asked for it on purpose
		// (tests, demos, plumbing exercises).  Keep a one-
		// line reminder so a stale terminal session that
		// still has --provider mock baked into a shell
		// alias doesn't get treated as "real assistant
		// said something useful".
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, c.warn("  (mock provider — replies are stub echoes; pass --provider openai for a real model)"))
	}
	fmt.Fprintln(w, "")
}

// displayOrUnset formats a banner value, substituting a
// dim "(provider default)" when the resolved field is the
// empty string (e.g. operator didn't override the model
// and the OpenAI default is going to kick in inside the
// provider).  Showing literally "" would look broken.
func displayOrUnset(v string) string {
	if v == "" {
		return "(provider default)"
	}
	return v
}

// bannerStyle renders banner fields with ANSI escapes when
// the writer is a real TTY and NO_COLOR / --no-color
// haven't disabled colour.  The zero-value (`useColor:
// false`) returns inputs unchanged, so non-TTY writers
// (pipes, file redirects, test buffers) get plain text.
type bannerStyle struct {
	useColor bool
}

func newBannerStyle(w io.Writer) bannerStyle {
	// Honour the no-color contract documented at https://no-color.org/
	// — any non-empty NO_COLOR env var disables ANSI output.
	if os.Getenv("NO_COLOR") != "" {
		return bannerStyle{}
	}
	if !isTerminal(w) {
		return bannerStyle{}
	}
	// TERM=dumb is the conventional opt-out for terminals
	// that don't render ANSI escapes (e.g. emacs M-x shell).
	if os.Getenv("TERM") == "dumb" {
		return bannerStyle{}
	}
	return bannerStyle{useColor: true}
}

func (b bannerStyle) wrap(code, s string) string {
	if !b.useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (b bannerStyle) label(s string) string { return b.wrap("36", s) } // cyan
func (b bannerStyle) value(s string) string { return b.wrap("1", s) }  // bold
func (b bannerStyle) dim(s string) string   { return b.wrap("2", s) }  // dim
func (b bannerStyle) warn(s string) string  { return b.wrap("33", s) } // yellow

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "  /help                  show this list")
	fmt.Fprintln(w, "  /exit, /quit, /q       leave the session")
	fmt.Fprintln(w, "  /clear                 drop conversation history (keep system prompt)")
	fmt.Fprintln(w, "  /show-context          running session metadata")
	fmt.Fprintln(w, "  /show-tools            tool surface available to the active skill")
	fmt.Fprintln(w, "  /show-skill            active skill name + version + source")
	fmt.Fprintln(w, "  /show-budget           tokens used + remaining")
	fmt.Fprintln(w, "  (anything else)        sent to the assistant as a user turn")
}

func printTools(w io.Writer, session *chat.Session) {
	if session.Tools == nil {
		fmt.Fprintln(w, "  (no tool registry)")
		return
	}
	all := session.Tools.All()
	if session.Skill != nil && len(session.Skill.Context.AvailableTools) > 0 {
		all = session.Tools.Filter(session.Skill.Context.AvailableTools)
	}
	for _, t := range all {
		marker := "  "
		if !t.ReadOnly() {
			marker = "! " // not surfaced to the model
		}
		fmt.Fprintf(w, "%s%s — %s\n", marker, t.Name(), t.Description())
	}
}

func printSkill(w io.Writer, skill *skills.Skill) {
	if skill == nil {
		fmt.Fprintln(w, "  (no active skill)")
		return
	}
	fmt.Fprintf(w, "  name:        %s\n", skill.Name)
	if skill.DisplayName != "" {
		fmt.Fprintf(w, "  display:     %s\n", skill.DisplayName)
	}
	fmt.Fprintf(w, "  version:     %s\n", skill.Version)
	fmt.Fprintf(w, "  source:      %s\n", skill.Source)
	fmt.Fprintf(w, "  read-only:   %v\n", skill.Permissions.ReadOnly)
	if len(skill.Context.AvailableTools) > 0 {
		fmt.Fprintf(w, "  tools:       %s\n", strings.Join(skill.Context.AvailableTools, ", "))
	}
}

// silence the unused-import linter on niche build paths.
var _ = llmprovider.DefaultRegistry
