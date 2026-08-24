# slop

> **THIS IS AN ABSOLUTE UNSIGMA SLOP FORK. IT MIGHT SUCK. IT IS NOT AN EVALUATOR.**

A deliberately tiny, mandatory-only Minishell differential tester rewritten in Go. It runs the Vienna corpus concurrently against your `minishell` and `bash --posix`, gives every case a stable ID, keeps optional Valgrind checks, and prints useful CI failures instead of walls of shell-script soup.

By default it uses every logical CPU reported by Go. This slop intentionally eats the runner so feedback arrives quickly. Set `--jobs N` on a shared runner.

Readline is not skipped. Real PTYs automate prompt and partial-line `Ctrl-C`, foreground-child `Ctrl-C`/`Ctrl-\\`, heredoc interruption/EOF, prompt EOF, history Up, and `Ctrl-L` redraw.

**Do not fail another student because slop says so.** Version-, order-, or environment-sensitive cases can be warnings. Inspect the supplied Bash/minishell outputs and reproduction commands.

## Install

Requires Go 1.22 or newer:

```sh
go install github.com/VeyasEvilBot/42_minishell_tester/cmd/slop@v0.1.1
```

Use the signed tag in CI instead of silently following the latest slop commit.

## Local usage

From the root of your Minishell repository:

```sh
make
slop ./minishell
slop --failures-only ./minishell
slop --jobs 4 --failures-only ./minishell
slop --leaks --failures-only ./minishell
slop --format json ./minishell > slop-results.json
slop --format jsonl ./minishell > slop-results.jsonl
slop --only 'readline.*' ./minishell
slop --only 'mand.12_vienna_eval.*' ./minishell
slop --ignore 'mand.10_parsing_hell.*' ./minishell
slop --list
```

Exit status:

- `0`: passes and warnings only;
- `1`: at least one real failure;
- `2`: bad CLI/config/setup.

Bonus is intentionally absent. There are intentionally **no unit tests**.

## GitHub Actions CI

Create `.github/workflows/slop.yml` in your Minishell repository:

```yaml
name: slop

on:
  push:
  pull_request:
  workflow_dispatch:

permissions:
  contents: read

jobs:
  mandatory-slop:
    runs-on: ubuntu-22.04
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4

      - uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5
        with:
          go-version: '1.22'

      - name: Build Minishell
        run: |
          sudo apt-get update
          sudo apt-get install -y --no-install-recommends build-essential libreadline-dev jq
          make fclean
          make EXTRA=0 -j"$(nproc)"

      - name: Install slop
        run: go install github.com/VeyasEvilBot/42_minishell_tester/cmd/slop@v0.1.1

      - name: Run once, retain JSON, show only useful cases
        shell: bash
        run: |
          set +e
          slop --format json ./minishell > slop-results.json
          status=$?
          jq -r '
            .results[]
            | select(.status != "pass")
            | "\n\(.status | ascii_upcase) \(.id) — \(.reason)\nminishell: \(.minishell_command)\nbash:      \(.bash_command)\nmini stdout:\n\(.minishell.stdout)\nmini stderr:\n\(.minishell.stderr)\nbash stdout:\n\(.bash.stdout)\nbash stderr:\n\(.bash.stderr)"
          ' slop-results.json
          exit "$status"

      - name: Upload structured slop result
        if: always()
        uses: actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02 # v4
        with:
          name: slop-results
          path: slop-results.json
```

This performs one tester run, stores the complete structured result, and prints only warnings/failures in the job log. Remove the artifact step if you want even less CI nonsense. Add `--jobs 4` if the runner should not be saturated.

For a leak job, install `valgrind` and add `--leaks`. Leak mode is much slower, so running it manually/nightly is less annoying than putting it on every push.

## 42 Austria Forgejo Actions CI

Your Minishell already uses Forgejo Actions from `.forgejo/workflows/ci.yml` with `ubuntu-latest`, `actions/checkout`, `apt`, `make`, and a shell test step. Add this as a separate `.forgejo/workflows/slop.yml`:

```yaml
name: slop

on:
  push:
  pull_request:
  workflow_dispatch:

permissions:
  contents: read

jobs:
  mandatory-slop:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Install Go
        uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Install dependencies and build
        shell: bash
        run: |
          sudo apt-get update
          sudo apt-get install -y --no-install-recommends \
            bash build-essential ca-certificates git jq libreadline-dev
          make fclean
          make EXTRA=0 -j"$(nproc)"
          go install github.com/VeyasEvilBot/42_minishell_tester/cmd/slop@v0.1.1

      - name: Run slop once
        shell: bash
        run: |
          set +e
          "$(go env GOPATH)/bin/slop" --format json ./minishell > slop-results.json
          status=$?
          jq -r '
            .results[]
            | select(.status != "pass")
            | "\n\(.status | ascii_upcase) \(.id) — \(.reason)\nminishell: \(.minishell_command)\nbash:      \(.bash_command)\nmini stdout:\n\(.minishell.stdout)\nmini stderr:\n\(.minishell.stderr)\nbash stdout:\n\(.bash.stdout)\nbash stderr:\n\(.bash.stderr)"
          ' slop-results.json
          exit "$status"
```

This mirrors the shape of your current 42 Austria workflow instead of relying on GitHub-only environment-summary hacks. The Forgejo runner must be allowed to fetch `actions/checkout`, `actions/setup-go`, Go modules, and the GitHub tester repository. If `setup-go` is disabled on the campus runner, install a Go 1.22+ package/image supplied by that runner administrator instead; Debian/Ubuntu's old Go package may be too old.

Your repository also has `.woodpecker/test.yml`. Woodpecker is a different runner format: use a `golang:1.22-bookworm` step, install `build-essential libreadline-dev jq`, build Minishell, then run the same `go install` and `slop --format json` commands. Do not paste Forgejo Actions YAML into `.woodpecker/`.

## `.sloprc` and goofy local tests

Put `.sloprc` in the Minishell repository root. CLI values override config values:

```toml
jobs = 12
timeout = "4s"
ignore = ["mand.10_parsing_hell.004"]
warn = ["goofy.export-order"]
strict_version = false

# Required before [tests] are accepted. These strings execute under BOTH
# trusted Bash and the Minishell being tested. Never enable this in an
# untrusted repository or pull request.
allow_tests = true

[tests."goofy.unset-path-absolute-command"]
input = "unset PATH\n/bin/echo absolute-still-works\n"

[tests."goofy.quoted-expansion"]
input = "export SLOP='hello goofy world'\necho \"<$SLOP>\"\n"

[tests."goofy.pipeline-status"]
input = "false | true\necho $?\ntrue | false\necho $?\n"

[tests."goofy.heredoc-expansion"]
input = "export SLOP=goofy\ncat << EOF\n<$SLOP>\nEOF\n"

# Useful, but export order is not stable enough to be an authoritative fail.
[tests."goofy.export-order"]
input = "export SLOP_Z=1\nexport SLOP_A=2\nexport\n"
warning = true
reason = "export listing order/format may differ"
```

A new ID adds a test. Reusing an embedded ID overwrites it. Every test has a deterministic ID; use `slop --list`. `--ignore`, `--only`, `ignore`, and `warn` accept exact IDs or shell globs.

Automatic `.sloprc` test execution is deliberately guarded by `allow_tests = true`: test input is executable shell code. Ignore/warn/job settings do not need that option.

## Failure and structured output

Each warning/failure contains:

- stable test ID and reason;
- Bash and GNU coreutils versions;
- bounded stdout/stderr and exit codes from both shells;
- copy/paste commands recreating the temporary work directory, `outfiles`, locale, and terminal environment;
- Valgrind output when requested.

Each process is time-limited, descendants are killed as a process group, and each output stream is capped at 1 MiB so a cursed shell cannot eat infinite memory.

`--failures-only` is the nicest human log. `--format json` emits one document; `--format jsonl` emits one result per line.

## Compatibility warnings

The reference baseline is Ubuntu 22.04: Bash `5.1.16` and GNU coreutils `8.32`. On another version, incorrect builtin-usage behavior where Bash exits `2` warns by default unless `--strict-version` is set. Tests explicitly marked with `warn` also remain non-fatal. A fresh second Bash run detects genuinely unstable cases, and pure line-order differences warn instead of pretending to be authoritative.

## Scope

The corpus contains the old mandatory cases plus automatable Vienna evaluation feedback. PTY cases use `readline.*` IDs and can be selected/ignored like everything else. No bonus, no unit-test cathedral, no evaluator claims.

**Again: this is an absolute unsigma personal slop fork. It might suck. Read the diff before trusting it.**
