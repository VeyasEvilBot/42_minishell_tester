# slop

> **THIS IS AN ABSOLUTE UNSIGMA SLOP FORK. IT MIGHT SUCK. IT IS NOT AN EVALUATOR.**

A deliberately tiny, mandatory-only Minishell differential tester rewritten in Go. It runs the old Vienna corpus concurrently against your `minishell` and `bash --posix`, gives every case a stable ID, keeps optional Valgrind checks, and prints useful CI failures instead of walls of shell-script soup. The default worker count is every logical CPU reported by Go: this absolute slop intentionally eats the box to finish fast.

Readline is not skipped. PTY-backed cases automate empty-prompt and partial-line `Ctrl-C`, foreground-child `Ctrl-C`/`Ctrl-\\`, heredoc interruption, heredoc EOF, prompt EOF, `Ctrl-C` followed by `Ctrl-D`, history Up, and `Ctrl-L` redraw. PTY cases share the same worker pool, so they run concurrently too.

This is personal tooling, not an evaluator. **Do not fail someone because slop says so.** Version-sensitive, nondeterministic, environment-order, and output-order differences are warnings unless `--strict-version` is set.

## Install

```sh
go install github.com/VeyasEvilBot/42_minishell_tester/cmd/slop@go-slop
```

When merged/tagged, replace `@go-slop` with `@latest`.

## Run

From the root of the Minishell repository:

```sh
slop
slop --jobs 12 --failures-only
slop --leaks --failures-only
slop --format json > slop-results.json
slop --ignore 'mand.10_parsing_hell.*' ./minishell
slop --only 'mand.12_vienna_eval.*' ./minishell
slop --list
```

Exit status is `0` with passes/warnings only, `1` for real failures, and `2` for CLI/setup errors. Bonus tests are intentionally absent.

## `.sloprc`

Put `.sloprc` in the root of the Minishell repo. CLI values win over config values.

```toml
jobs = 12
timeout = "4s"
ignore = ["mand.10_parsing_hell.004", "mand.1_builtins_echo.0*"]
warn = ["mand.2_path_check.*"]
strict_version = false

# Add a test with a new ID, or overwrite an embedded test by reusing its ID.
[tests."local.export-keeps-value"]
input = "export A=hello\nexport A\necho $A\n"

[tests."mand.1_builtins_exit.001"]
input = "exit 0\n"
warning = true
reason = "local implementation intentionally differs"
```

IDs are deterministic: `mand.<source-file>.<three-digit-block-number>`. Use `slop --list` to discover them. `--ignore` and `--only` accept comma-separated IDs or shell globs.

## Failure output

Each failure/warning contains:

- stable test ID and reason;
- Bash and GNU coreutils versions;
- complete stdout, stderr, and exit codes from both shells;
- copy/paste `printf ... | ./minishell` and `printf ... | bash --posix` verification commands;
- Valgrind output when `--leaks` finds a problem.

`--failures-only` hides passing cases. `--format json` emits one structured result document; `--format jsonl` emits one result per line for CI ingestion.

## Compatibility warnings

The reference baseline is Ubuntu 22.04: Bash `5.1.16` and GNU coreutils `8.32`. On another version, likely version-sensitive differences—especially Bash exit `2` for incorrect builtin usage—warn rather than fail by default. Run Bash twice: if Bash disagrees with itself or only line order differs, slop also warns instead of pretending the test is authoritative.

## Scope

The corpus includes the original mandatory tests plus automatable cases recovered from Vienna evaluation feedback. Interactive Readline, signal, terminal-redraw, child-interruption, and heredoc-interruption checks use real pseudo-terminals instead of pretending piped stdin is interactive. All PTY cases have `readline.*` IDs and can be selected or ignored like other cases.

There are intentionally **no unit tests**. CI builds the tiny CLI and smoke-runs one embedded case. This remains an absolute unsigma slop fork and might suck.
