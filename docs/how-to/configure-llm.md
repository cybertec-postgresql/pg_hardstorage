# Configure the LLM helper

`pg_hardstorage llm` (and the no-subcommand chat shortcut)
talks to any **OpenAI-compatible Chat Completions endpoint**:
api.openai.com, Anthropic via OpenAI-compat, Azure OpenAI,
Ollama (local), vLLM, OpenRouter, LiteLLM. One wire format,
one set of code, one set of tests — pick your backend and
point the CLI at it.

If nothing is configured, `pg_hardstorage llm` **refuses
to start** with the structured error
`llm.no_provider_configured` — the silent fallback to the
mock provider that earlier versions had was removed in
v0.10 (it caused hours of typing real questions into stub
replies that *looked* convincing).  The error message
prints the exact env vars to set; pick one of the recipes
below and try again.

The mock provider remains available via explicit
`--provider mock` for tests, demos, and plumbing
exercises.  The chat banner shows `provider: mock` so it's
unmistakable.

When you do connect to a real provider, the chat banner
shows the resolved **endpoint URL** and **model name** on
their own lines — the two questions an operator asks
first when a chat seems off (*"am I pointed at the right
host?"*, *"which model is answering?"*).  Output is
auto-colourised on a TTY and degrades to plain text on
pipes / file redirects / `NO_COLOR=1` / `TERM=dumb`.

## Quickest path: env vars

The two canonical env vars (project-branded):

```bash
export PG_HARDSTORAGE_LLM_KEY='sk-proj-...'
export PG_HARDSTORAGE_URL='https://api.openai.com'   # optional; OpenAI default
pg_hardstorage llm
```

That's it. The banner should now read `provider: openai`
with the resolved `model:` and `url:` shown beneath.

`OPENAI_API_KEY` and `OPENAI_BASE_URL` are also accepted (the
cross-tool standard, recognised for ergonomics when the same
keys drive other CLIs on the box). Precedence:

| Field | Order |
|---|---|
| **endpoint** | `--endpoint` flag → `$PG_HARDSTORAGE_URL` → `$OPENAI_BASE_URL` → `llm.endpoint` in YAML → provider default |
| **api key** | `$PG_HARDSTORAGE_LLM_KEY` → `$OPENAI_API_KEY` → `$PG_HARDSTORAGE_LLM_API_KEY` (legacy) → `llm.api_key_file` → `llm.api_key` |
| **model** | `--model` flag → `$PG_HARDSTORAGE_LLM_MODEL` → `$OPENAI_MODEL` → `llm.model` in YAML → provider default (gpt-4o-mini) |

⚠ The default model `gpt-4o-mini` is **OpenAI-specific**. When
`PG_HARDSTORAGE_URL` points at a non-OpenAI endpoint
(Anthropic via OpenAI-compat, Azure OpenAI, Ollama, vLLM,
OpenRouter), you MUST set `PG_HARDSTORAGE_LLM_MODEL` (or
`--model`, or `llm.model` in the YAML) to a model the
endpoint exposes — otherwise the upstream returns a 404
that the CLI surfaces with an actionable hint naming the
env var:

```
ERROR openai: status 404: Model with name gpt-4o-mini does
not exist — model "gpt-4o-mini" not available at this
endpoint; set PG_HARDSTORAGE_LLM_MODEL to a model the
endpoint exposes (or pass --model on the CLI). Examples:
openai → gpt-4o-mini / gpt-4o; anthropic-via-compat →
claude-3-5-sonnet-20241022; ollama → whatever you've pulled
(llama3.1:8b, mistral, ...)
```

## Persistent: YAML config

Drop a config file at one of the standard paths:

```bash
mkdir -p ~/.config/pg_hardstorage
cat > ~/.config/pg_hardstorage/pg_hardstorage.yaml <<'YAML'
schema: pg_hardstorage.config.v1

llm:
  provider: openai
  endpoint: https://api.openai.com
  model: gpt-4o-mini
  api_key_file: /home/me/.config/pg_hardstorage/openai.key
YAML

# Key on a separate file with mode 0600 — survives YAML
# version control, rotation just rewrites the key file.
install -m 0600 /dev/null ~/.config/pg_hardstorage/openai.key
$EDITOR ~/.config/pg_hardstorage/openai.key   # paste the key
```

System-wide install for multi-operator boxes:

```bash
sudo install -d -m 0755 /etc/pg_hardstorage
sudo tee /etc/pg_hardstorage/pg_hardstorage.yaml > /dev/null <<'YAML'
schema: pg_hardstorage.config.v1
llm:
  provider: openai
  model: gpt-4o-mini
  api_key_file: /etc/pg_hardstorage/openai.key
YAML
sudo install -m 0600 /dev/null /etc/pg_hardstorage/openai.key
sudo $EDITOR /etc/pg_hardstorage/openai.key
```

The `api_key_file` indirection is preferred over the inline
`api_key` field — the file is **re-read on every CLI
invocation**, so a key rotation is just `echo new-key >
openai.key` with no service restart.

A fully annotated reference config ships at
`share/pg_hardstorage.sample.yaml`. Copy and edit.

## Recipes for common backends

### OpenAI (default)

```bash
export PG_HARDSTORAGE_LLM_KEY='sk-proj-...'
pg_hardstorage llm
```

The default model `gpt-4o-mini` (cheap, ~$0.15/M tokens) is
right for the operator-assistant traffic this CLI generates.
Override for higher-quality answers:

```bash
export PG_HARDSTORAGE_LLM_MODEL=gpt-4o     # ~$2.50/M, better
# or:
export PG_HARDSTORAGE_LLM_MODEL=gpt-4.1    # best
```

### Anthropic Claude (via Anthropic's OpenAI-compat shim)

```bash
export PG_HARDSTORAGE_LLM_KEY='sk-ant-...'
export PG_HARDSTORAGE_URL='https://api.anthropic.com/v1'
export PG_HARDSTORAGE_LLM_MODEL='claude-3-5-sonnet-20241022'
pg_hardstorage llm
```

### Local Ollama (air-gap-friendly)

```bash
ollama pull llama3.1:8b           # one-time
export PG_HARDSTORAGE_URL='http://127.0.0.1:11434/v1'
export PG_HARDSTORAGE_LLM_KEY='ollama'   # any non-empty value
export PG_HARDSTORAGE_LLM_MODEL='llama3.1:8b'
pg_hardstorage llm
```

The fact that an air-gap-friendly local model "just works"
through the same code path as OpenAI is the point of routing
every backend through the OpenAI-compat shape.

### Azure OpenAI

```yaml
llm:
  provider: openai
  endpoint: https://your-resource.openai.azure.com/openai/deployments/your-deployment
  api_key_file: /etc/pg_hardstorage/azure.key
  extra:
    api_key_header: api-key   # Azure's auth header is "api-key", not "Authorization"
```

### OpenRouter / LiteLLM / vLLM

Same shape — `endpoint:` at the service URL, `api_key:` if
required, `model:` to whatever the service exposes.

## Verifying it works

```bash
pg_hardstorage llm ask "summarise pg_hardstorage backup in two sentences"
```

A real response is short, grammatical, and names actual
concepts (CAS, manifest, FastCDC chunking). The mock
provider echoes your prompt back as `mock-reply: …`.

Common errors:

- `401 Incorrect API key` — the key didn't authenticate; re-check.
- `429 Too Many Requests` — rate limit / billing.
- `model not found` — the chosen model isn't on your account; try `gpt-4o-mini`.
- `connection refused` (Ollama) — `ollama serve` not running, or wrong port.

## Reasoning-model output

Reasoning models (OpenAI o1/o3/o4, DeepSeek-R1, Gemini-think,
Claude with extended thinking) emit chain-of-thought
content that should not be shown to operators.  The
`pg_hardstorage llm` provider strips it from the visible
stream:

- **Inline `<think>...</think>` and `<thinking>...</thinking>`
  blocks** are scrubbed even when tag boundaries straddle
  stream chunks (the filter buffers across SSE events).
- **The OpenAI-spec `reasoning_content` delta field** is
  consumed and dropped — only the `content` field reaches
  the operator.

You don't have to do anything to enable this; it's the
default for every backend.

## Air-gap mode

If `airgapped: strict` is set in pg_hardstorage.yaml or
`PG_HARDSTORAGE_AIRGAPPED=1` is exported, the LLM
provider's endpoint is gated through the airgap allowlist
**before** any HTTP request is made. A public endpoint
(api.openai.com) refused here fails fast with a wrapped
`airgap.ErrEndpointNotAllowed` instead of silently
producing one TCP connection per Chat call.

For air-gapped deployments, configure Ollama or vLLM at
loopback or RFC1918 and the LLM helper works the same way
the connected installs do.

## Hallucination resistance

The LLM helper has four layers of defence against the
single most common failure mode — the model suggesting a
command that doesn't exist (e.g. `pg_hardstorage
deployment create --name X` when the real shape is
`deployment add <name> --connection ... --repo ...`):

1. **Command catalog in the system prompt.** Every
   session bootstrap injects a depth-2 rendering of the
   live cobra command tree into the system prompt.  The
   model sees the real verbs before it answers.
2. **`read_command_help` tool.** When the model needs
   the flag list for a specific command (the catalog
   doesn't include flags), it calls
   `read_command_help` to look up synopsis + flag list
   from the live tree.
3. **`suggest_command` validation.** When the model
   uses the `suggest_command` tool, the proposed
   command is parsed against the live cobra tree.
   Unknown subcommands or flags are returned as a
   structured tool error with a did-you-mean hint, so
   the model retries with the right shape instead of
   dumping the bad command at the operator.
4. **Post-response backtick scrubber.** When the model
   writes a command in plain prose (where the tool gate
   doesn't fire), the chat REPL scans the response for
   backtick-wrapped `pg_hardstorage` commands and
   surfaces a `⚠ command-validation warnings` block
   when any of them don't parse.

The four layers compose: the catalog grounds the model,
`read_command_help` gives lazy detail lookup,
`suggest_command` validation hard-stops bad
tool-mediated suggestions, and the scrubber catches
prose-inlined ones.

### Nightly eval (opt-in)

A small nightly canary lives at
`internal/cli/llm_eval_test.go` (test name
`TestLLMEval_RealProviderHallucinationResistance`).  It
issues four "how do I X?" prompts to a real provider
and asserts that any command the model produces — via
`suggest_command` or in prose — parses against the
live cobra tree.  Run it on demand:

```bash
PG_HARDSTORAGE_LLM_EVAL=1 \
PG_HARDSTORAGE_LLM_KEY=sk-... \
go test ./internal/cli/ -run TestLLMEval -v
```

Default CI skips this test (it requires network +
billable tokens).  Use it as a regression check before
shipping a prompt or skill change.

## Privacy posture

`llm.privacy: redact` runs the LLM input/output through the
same redactor that scrubs DSNs / IPs / secrets from log
output. Useful when the assistant's history is going to
disk and you don't want pasted credentials sitting in
`~/.local/state/pg_hardstorage/llm-history`.

`llm.privacy: off` disables history entirely.

Default (empty): history is kept verbatim. Acceptable for
single-operator dev boxes; not recommended for production.
