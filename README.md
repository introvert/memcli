# memcli

`mem` — a tiny personal memory for the terminal. Push notes in a second, find them later
with full-text search, or just ask in plain language and let an AI answer from your notes.

```
$ mem "staging db: pg-staging.internal:5432, user app_ro #work #infra"
saved 261025-235030
$ mem s staging
261025-235030  2026-10-25  staging db: pg-staging.internal:5432, user app_ro  #infra #work
$ mem ask "what are the staging db credentials?"
· search "staging database" → 2
· read mem/261025-235030
Host pg-staging.internal:5432, user app_ro [mem/261025-235030]. The password is redacted;
run: mem show mem/261025-235030

sources:
  mem/261025-235030  staging db: pg-staging.internal:5432, user app_ro
```

(Illustrative output.) `ask` runs a small agent that searches, greps and reads your notes —
and a privacy filter makes sure passwords, tokens and keys never reach the model.

## Install

macOS / Linux, no Go needed:

```sh
curl -fsSL https://raw.githubusercontent.com/introvert/memcli/main/install.sh | sh
```

It downloads the right binary from [Releases](https://github.com/introvert/memcli/releases),
verifies its SHA-256 checksum, and installs to `~/.local/bin`
(`MEM_INSTALL_DIR=...` to change, `MEM_VERSION=v0.1.0` to pin). Run it again to update.
Windows: download the `.zip` from Releases.

With Go:

```sh
go install github.com/introvert/memcli/cmd/mem@latest
```

Point it at your notes folder (optional — default is `~/.mem/notes`):

```sh
mem config        # creates ~/.mem/config.json; set "notes_dir": "~/code/notes", "inbox": "mem"
```

Optional: `git init` your notes folder to get version history (and sync) for free.

### Releasing

Push a tag — `git tag v0.1.0 && git push origin v0.1.0` — and the
[release workflow](.github/workflows/release.yml) builds macOS, Linux and Windows binaries
(amd64 + arm64) and publishes them with checksums.

## Usage

```
mem buy oat milk #todo          add a note (bare text = add; #hashtags become tags)
echo "..." | mem                add from stdin
mem -e                          add in $EDITOR
mem add -a "..."                add and let AI pick tags
mem s oat milk [-t todo]        search (ranked, prefix-matching)
mem ask "question"              AI agent searches your notes and answers, citing sources
mem secrets                     what the privacy filter hides from AI (values masked)
mem ls [-n 20] [-t todo]        recent notes
mem show ID                     print a note (ID prefix is enough)
mem edit ID                     open in $EDITOR
mem rm ID                       move to trash
mem tags                        tag counts
mem config                      show/create config
mem path | reindex              storage dir | rebuild index
```

`--json` on `s`, `ls`, `show` and `ask` gives machine-readable output, so scripts and AI agents
can use the same memory. `-m provider/model` picks a model for one run.

> Shell tip: bash (and zsh with `interactivecomments`) treats an unquoted `#tag` as a comment.
> Quote the note — `mem "foo #tag"` — or use `-t tag`.

## How it works

- **Notes are plain markdown files** in any folder you choose — `notes_dir` in the config
  (default `~/.mem/notes`), e.g. an existing Obsidian vault. Every `.md` file in it is indexed,
  recursively (hidden folders like `.obsidian` are skipped); new notes go to the `inbox`
  subfolder with a small frontmatter (`created`, `tags: [a, b]`). Notes are addressed by path,
  e.g. `mem show Projects/ideas` or just `mem show ideas`. The files are the source of truth:
  grep them, edit them in Obsidian, sync them however you like.
- **Config and index** live in `~/.mem` (`$MEM_HOME`), never inside your notes folder.
- **Search** uses a SQLite FTS5 index (`~/.mem/index.db`) with BM25 ranking — title matches weigh
  most, then tags, then body — plus prefix matching, stemming and accent folding. The index is a
  cache: it refreshes from file mtimes on every run, and deleting it loses nothing.
- **Git**: if the notes folder is in a git repo, every add/edit/rm is committed automatically.
  `rm` moves notes to `.trash/` in the notes folder (the same place Obsidian uses).
- **`mem ask` is an agent.** The model gets four tools — `search` (ranked full-text), `grep`
  (exact text/regex, returns matching lines), `list` (folders, tags, recent notes) and `read`
  (a note with line numbers) — and decides what to look up: synonyms, other languages, exact
  strings, then reads the notes it needs. Each step is shown as it runs; it answers within
  `max_steps` rounds and cites the notes it used. Only what the tools return is sent, never the
  whole folder.
- **Secrets never leave your machine.** Everything the model sees passes through a redaction
  filter: known token formats (AWS, GitHub, GitLab, Slack, Stripe, OpenAI/Anthropic/DeepSeek-style
  `sk-…`, Google, JWT, SendGrid, npm/PyPI/Hugging Face, Telegram), private-key blocks, passwords in
  URLs, values after labels like `password:`, `token=`, `api_key`, `secret`, `geslo`, crypto seed
  phrases (BIP-39) and private keys, card numbers, and unknown high-entropy keys. The model sees
  `[REDACTED:kind]` and can point you to the note; `mem show` displays the real value locally.
  The search index itself stores only redacted text, so secrets can't be probed by searching for
  guesses either. Run `mem secrets` to review what is caught; add your own patterns with
  `redact_patterns`.
- **AI is optional.** Adding, searching and listing work offline; only `ask` and `-a` call a model.

## Models

Default: `deepseek/deepseek-flash` (set `DEEPSEEK_API_KEY`). Any model works, written as
`provider/model`:

| Provider | Example | Key |
|---|---|---|
| deepseek | `deepseek/deepseek-flash` | `DEEPSEEK_API_KEY` |
| anthropic | `anthropic/claude-opus-5-5` | `ANTHROPIC_API_KEY` |
| openai | `openai/<model>` | `OPENAI_API_KEY` |
| gemini | `gemini/<model>` | `GEMINI_API_KEY` |
| openrouter | `openrouter/<vendor>/<model>` | `OPENROUTER_API_KEY` |
| groq, mistral, xai | `groq/<model>` … | `GROQ_API_KEY` … |
| ollama, lmstudio | `ollama/llama3.2` | — (local) |

Claude uses the official Anthropic Go SDK; everything else uses the OpenAI-compatible
chat completions API, so any compatible server can be added in `~/.mem/config.json`:

```json
{
  "notes_dir": "~/code/notes",
  "inbox": "mem",
  "model": "deepseek/deepseek-flash",
  "fast_model": "",
  "auto_tag": false,
  "max_steps": 12,
  "redact_patterns": ["INTERNAL-\\d{6}"],
  "providers": {
    "my-server": { "api": "openai", "base_url": "http://localhost:8080/v1", "api_key_env": "MY_KEY" },
    "deepseek":  { "options": { "reasoning_effort": "high" } }
  }
}
```

- `fast_model` handles small jobs (tagging); defaults to `model`. The agent needs a model with
  tool/function calling (DeepSeek, Claude, OpenAI, Gemini, most Ollama models like `qwen3`, `llama3.1+`).
- `max_steps`: tool-call rounds per `ask` before the model must answer.
- `redact_patterns`: extra regexes to hide from the model, on top of the built-in filter.
- `options` are merged into every request body; `small_options` on top of them for small jobs
  (`null` removes a key). Built-in DeepSeek defaults: low reasoning effort for the agent, thinking off
  for small jobs.
- `MEM_MODEL=...` / `MEM_NOTES=...` override the model / notes folder for a shell session.
- Privacy: the agent sends the model only what its tools return, after redaction. Use a local
  model (`ollama/...`) if even redacted notes must not leave your machine.
