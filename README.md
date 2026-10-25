# memcli

`mem` — a tiny personal memory for the terminal. Push notes in a second, find them later
with full-text search, or just ask in plain language and let an AI answer from your notes.

```
$ mem "staging db: pg-staging.internal:5432, user app_ro #work #infra"
saved 261025-235030
$ mem s staging
261025-235030  2026-10-25  staging db: pg-staging.internal:5432, user app_ro  #infra #work
$ mem ask "what's the staging db host?"
pg-staging.internal:5432, user app_ro [261025-235030].

sources:
  261025-235030  staging db: pg-staging.internal:5432, user app_ro
```

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
mem ask "question"              AI answer from your notes, citing sources
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
- **`mem ask`** sends all your notes to the model in one call while they fit in `context_budget`
  (best recall). Notes go first, oldest first, so providers' prompt caching reuses that prefix
  between questions. Past the budget it falls back to search, widened with AI-suggested keywords.
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
  "context_budget": 400000,
  "providers": {
    "my-server": { "api": "openai", "base_url": "http://localhost:8080/v1", "api_key_env": "MY_KEY" },
    "deepseek":  { "options": { "reasoning_effort": "high" } }
  }
}
```

- `fast_model` handles small jobs (tagging, keyword expansion); defaults to `model`.
- `options` are merged into every request body; `small_options` on top of them for small jobs
  (`null` removes a key). Built-in DeepSeek defaults: low reasoning effort for `ask`, thinking off
  for small jobs.
- Lower `context_budget` (e.g. `20000`) for local models with small context windows.
- `MEM_MODEL=...` / `MEM_NOTES=...` override the model / notes folder for a shell session.
- Privacy: `mem ask` sends your notes (all of them, while under `context_budget`) to the
  configured provider. Use a local model (`ollama/...`) if they must not leave your machine.
