// slop is the absolute unsigma slop-fork minishell tester. It might suck.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed corpus/cmds/mand/*.sh corpus/minishell.supp
var corpus embed.FS

type config struct {
	Jobs          int                   `toml:"jobs"`
	Timeout       string                `toml:"timeout"`
	Ignore        []string              `toml:"ignore"`
	Warn          []string              `toml:"warn"`
	StrictVersion bool                  `toml:"strict_version"`
	AllowTests    bool                  `toml:"allow_tests"`
	Tests         map[string]configTest `toml:"tests"`
}

type configTest struct {
	Input   string `toml:"input"`
	Warning bool   `toml:"warning"`
	Reason  string `toml:"reason"`
}

type testCase struct {
	ID, Input, Source, WarningReason string
	Scenario                         string
}

type runOutput struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	StartError string `json:"start_error,omitempty"`
}

const maxCaptureBytes = 1 << 20

type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := maxCaptureBytes - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		_, _ = b.buf.Write(p)
	} else {
		b.truncated = true
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if b.truncated {
		s += "\n[SLOP OUTPUT TRUNCATED AT 1 MiB]"
	}
	return s
}

func (b *boundedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

type result struct {
	ID             string    `json:"id"`
	Status         string    `json:"status"`
	Reason         string    `json:"reason,omitempty"`
	Input          string    `json:"input"`
	Source         string    `json:"source"`
	Minishell      runOutput `json:"minishell"`
	Bash           runOutput `json:"bash"`
	BashVersion    string    `json:"bash_version,omitempty"`
	Coreutils      string    `json:"coreutils_version,omitempty"`
	MinishellCmd   string    `json:"minishell_command,omitempty"`
	BashCmd        string    `json:"bash_command,omitempty"`
	LeakOutput     string    `json:"leak_output,omitempty"`
	DurationMillis int64     `json:"duration_ms"`
}

type options struct {
	target, format, configPath, ignoreCSV, onlyCSV string
	jobs                                           int
	timeout                                        time.Duration
	failuresOnly, leaks, list, strict              bool
	bashVersion, coreutils                         string
	versionDrift                                   bool
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
var bashPrefixRE = regexp.MustCompile(`(?m)^bash: (line [0-9]+: )?`)
var shellPrefixRE = regexp.MustCompile(`(?m)^[^:\n]*minishell[^:\n]*: (line [0-9]+: )?`)

func main() {
	var o options
	defaultJobs := runtime.NumCPU()
	flag.StringVar(&o.configPath, "config", ".sloprc", "TOML config (missing default is fine)")
	flag.IntVar(&o.jobs, "jobs", defaultJobs, "parallel test workers")
	flag.DurationVar(&o.timeout, "timeout", 4*time.Second, "timeout per shell invocation")
	flag.BoolVar(&o.failuresOnly, "failures-only", false, "print only failures/warnings and summary")
	flag.BoolVar(&o.leaks, "leaks", false, "run valgrind leak checks")
	flag.StringVar(&o.format, "format", "text", "text, json, or jsonl")
	flag.StringVar(&o.ignoreCSV, "ignore", "", "comma-separated test IDs/globs to ignore")
	flag.StringVar(&o.onlyCSV, "only", "", "comma-separated test IDs/globs to run")
	flag.BoolVar(&o.list, "list", false, "list test IDs without running")
	flag.BoolVar(&o.strict, "strict-version", false, "fail version-sensitive differences instead of warning")
	flag.Usage = usage
	flag.Parse()
	if flag.NArg() > 1 {
		usage()
		os.Exit(2)
	}
	o.target = "./minishell"
	if flag.NArg() == 1 {
		o.target = flag.Arg(0)
	}

	cfg, err := loadConfig(o.configPath)
	fatalIf(err)
	if cfg.Jobs > 0 && !wasFlagSet("jobs") {
		o.jobs = cfg.Jobs
	}
	if cfg.Timeout != "" && !wasFlagSet("timeout") {
		o.timeout, err = time.ParseDuration(cfg.Timeout)
		fatalIf(err)
	}
	if cfg.StrictVersion && !wasFlagSet("strict-version") {
		o.strict = true
	}
	if o.jobs < 1 {
		fatalIf(errors.New("jobs must be at least 1"))
	}
	if o.timeout <= 0 {
		fatalIf(errors.New("timeout must be greater than zero"))
	}
	if o.format != "text" && o.format != "json" && o.format != "jsonl" {
		fatalIf(errors.New("format must be text, json, or jsonl"))
	}

	tests, err := loadTests(cfg)
	fatalIf(err)
	ignore := append(append([]string{}, cfg.Ignore...), splitCSV(o.ignoreCSV)...)
	only := splitCSV(o.onlyCSV)
	fatalIf(validatePatterns(ignore, only, cfg.Warn))
	tests = filterTests(tests, ignore, only, cfg.Warn)
	if o.list {
		for _, t := range tests {
			fmt.Printf("%s\t%s\n", t.ID, firstLine(t.Input))
		}
		return
	}
	abs, err := filepath.Abs(o.target)
	fatalIf(err)
	o.target = abs
	st, err := os.Stat(o.target)
	if err != nil || st.IsDir() || st.Mode()&0111 == 0 {
		fatalIf(fmt.Errorf("minishell executable not found: %s", o.target))
	}

	o.bashVersion = commandVersion("bash", "--version")
	o.coreutils = commandVersion("env", "--version")
	o.versionDrift = !strings.Contains(o.bashVersion, "5.1.16") || !strings.Contains(o.coreutils, "8.32")
	if o.leaks {
		if _, err := exec.LookPath("valgrind"); err != nil {
			fatalIf(errors.New("--leaks requested but valgrind is not installed"))
		}
	}
	results := runAll(tests, o)
	printResults(results, o)
	for _, r := range results {
		if r.Status == "fail" {
			os.Exit(1)
		}
	}
}

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), `slop — absolute unsigma mandatory-minishell slop tester (might suck)

Usage: slop [flags] [./minishell]

`)
	flag.PrintDefaults()
}

func loadConfig(path string) (config, error) {
	var c config
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) && path == ".sloprc" {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	_, err = toml.DecodeFile(path, &c)
	if err == nil && len(c.Tests) > 0 && !c.AllowTests {
		return c, errors.New(".sloprc [tests] execute under bash/minishell; set allow_tests = true only for a trusted repository")
	}
	return c, err
}

func loadTests(c config) ([]testCase, error) {
	entries, err := fs.Glob(corpus, "corpus/cmds/mand/*.sh")
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	byID := map[string]testCase{}
	order := []string{}
	for _, name := range entries {
		b, err := corpus.ReadFile(name)
		if err != nil {
			return nil, err
		}
		stem := strings.TrimSuffix(filepath.Base(name), ".sh")
		blocks := parseBlocks(string(b))
		for i, input := range blocks {
			id := fmt.Sprintf("mand.%s.%03d", sanitizeID(stem), i+1)
			byID[id] = testCase{ID: id, Input: ensureNewline(input), Source: name}
			order = append(order, id)
		}
	}
	for _, t := range interactiveTests() {
		byID[t.ID] = t
		order = append(order, t.ID)
	}
	keys := make([]string, 0, len(c.Tests))
	for id := range c.Tests {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		v := c.Tests[id]
		if strings.TrimSpace(v.Input) == "" {
			return nil, fmt.Errorf("test %s has empty input", id)
		}
		if _, ok := byID[id]; !ok {
			order = append(order, id)
		}
		byID[id] = testCase{ID: id, Input: ensureNewline(v.Input), Source: ".sloprc", WarningReason: warningReason(v)}
	}
	out := make([]testCase, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

func warningReason(v configTest) string {
	if !v.Warning {
		return ""
	}
	if v.Reason != "" {
		return v.Reason
	}
	return "configured warning"
}

func parseBlocks(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []string
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		v := strings.TrimSpace(strings.Join(cur, "\n"))
		if v != "" {
			out = append(out, v)
		}
		cur = nil
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

func filterTests(in []testCase, ignore, only, warn []string) []testCase {
	out := make([]testCase, 0, len(in))
	for _, t := range in {
		if matchesAny(t.ID, ignore) || (len(only) > 0 && !matchesAny(t.ID, only)) {
			continue
		}
		if t.WarningReason == "" && matchesAny(t.ID, warn) {
			t.WarningReason = "configured warning"
		}
		out = append(out, t)
	}
	return out
}

func matchesAny(id string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == id {
			return true
		}
		if ok, _ := filepath.Match(p, id); ok {
			return true
		}
	}
	return false
}

func validatePatterns(groups ...[]string) error {
	for _, patterns := range groups {
		for _, pattern := range patterns {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			if _, err := filepath.Match(pattern, "test"); err != nil {
				return fmt.Errorf("invalid test glob %q: %w", pattern, err)
			}
		}
	}
	return nil
}

func runAll(tests []testCase, o options) []result {
	jobs := make(chan testCase)
	results := make(chan result, len(tests))
	var done atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < o.jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				results <- runOne(t, o)
				if o.format == "text" && !o.failuresOnly && isTerminal(os.Stderr.Fd()) {
					n := done.Add(1)
					fmt.Fprintf(os.Stderr, "\rslopping %d/%d", n, len(tests))
				}
			}
		}()
	}
	go func() {
		for _, t := range tests {
			jobs <- t
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	out := make([]result, 0, len(tests))
	for r := range results {
		out = append(out, r)
	}
	if o.format == "text" && !o.failuresOnly && isTerminal(os.Stderr.Fd()) {
		fmt.Fprintln(os.Stderr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func runOne(t testCase, o options) result {
	if t.Scenario != "" {
		return runInteractiveOne(t, o)
	}
	start := time.Now()
	base, err := os.MkdirTemp("", "slop-*")
	if err != nil {
		return testerErrorResult(t, err)
	}
	defer os.RemoveAll(base)
	miniDir := filepath.Join(base, "mini")
	bashDir := filepath.Join(base, "bash")
	bashAgainDir := filepath.Join(base, "bash-again")
	for _, dir := range []string{miniDir, bashDir, bashAgainDir} {
		if err := os.MkdirAll(filepath.Join(dir, "outfiles"), 0755); err != nil {
			return testerErrorResult(t, err)
		}
	}
	mini := runCommand(o.timeout, miniDir, os.Environ(), t.Input, o.target)
	bash := runCommand(o.timeout, bashDir, os.Environ(), t.Input, "bash", "--posix")

	mini.Stdout = normalize(mini.Stdout, miniDir)
	mini.Stdout = stripInputEcho(mini.Stdout, t.Input)
	mini.Stderr = normalizeStderr(mini.Stderr, miniDir)
	bash.Stdout = normalize(bash.Stdout, bashDir)
	bash.Stderr = normalizeStderr(bash.Stderr, bashDir)

	r := result{ID: t.ID, Input: t.Input, Source: t.Source, Minishell: mini, Bash: bash, DurationMillis: time.Since(start).Milliseconds()}
	different := mini.Stdout != bash.Stdout || mini.Stderr != bash.Stderr || mini.ExitCode != bash.ExitCode || mini.TimedOut != bash.TimedOut
	bashUnstable := false
	if different {
		bashAgain := runCommand(o.timeout, bashAgainDir, os.Environ(), t.Input, "bash", "--posix")
		bashAgain.Stdout = normalize(bashAgain.Stdout, bashAgainDir)
		bashAgain.Stderr = normalizeStderr(bashAgain.Stderr, bashAgainDir)
		bashUnstable = bash.Stdout != bashAgain.Stdout || bash.Stderr != bashAgain.Stderr || bash.ExitCode != bashAgain.ExitCode
	}
	if !different {
		r.Status = "pass"
	} else {
		r.Status = "fail"
		r.Reason = differenceReason(mini, bash)
	}
	if mini.TimedOut {
		r.Status = "fail"
		r.Reason = "minishell timed out"
	}
	if bash.TimedOut {
		r.Status = "warning"
		r.Reason = "bash timed out; test is not reliable"
	}
	if bashUnstable {
		r.Status = "warning"
		r.Reason = "bash produced different results twice; order/environment-sensitive test"
	} else if r.Status == "fail" && sameWhenSorted(mini.Stdout, bash.Stdout) && mini.Stderr == bash.Stderr && mini.ExitCode == bash.ExitCode {
		r.Status = "warning"
		r.Reason = "only output order differs"
	} else if r.Status == "fail" && t.WarningReason != "" {
		r.Status = "warning"
		r.Reason = t.WarningReason
	} else if r.Status == "fail" && o.versionDrift && !o.strict && versionSensitive(t.Input, bash) {
		r.Status = "warning"
		r.Reason = "version-sensitive bash/coreutils behavior (Ubuntu 22.04 baseline is bash 5.1.16/coreutils 8.32)"
	}
	if r.Status != "pass" {
		r.BashVersion, r.Coreutils = o.bashVersion, o.coreutils
		r.MinishellCmd = verifyCommand(o.target, t.Input)
		r.BashCmd = verifyCommand("bash --posix", t.Input)
	}
	if o.leaks {
		leak, output := runLeak(o, base, t.Input)
		if leak {
			r.Status = "fail"
			r.Reason = joinReason(r.Reason, "valgrind reported leaks/errors")
			r.LeakOutput = output
		}
	}
	r.DurationMillis = time.Since(start).Milliseconds()
	return r
}

func runCommand(timeout time.Duration, dir string, env []string, input, name string, args ...string) runOutput {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, append(env, "LC_ALL=C", "TERM=dumb"), strings.NewReader(input)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return runOutput{ExitCode: 127, StartError: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-timer.C:
		timedOut = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = 127
		}
	}
	if timedOut {
		code = 124
	}
	return runOutput{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, TimedOut: timedOut}
}

func runLeak(o options, base, input string) (bool, string) {
	supp := filepath.Join(base, "minishell.supp")
	b, err := corpus.ReadFile("corpus/minishell.supp")
	if err != nil {
		return true, "cannot read valgrind suppressions: " + err.Error()
	}
	if err := os.WriteFile(supp, b, 0600); err != nil {
		return true, "cannot write valgrind suppressions: " + err.Error()
	}
	dir := filepath.Join(base, "leak")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return true, "cannot create valgrind workdir: " + err.Error()
	}
	out := runCommand(o.timeout*3, dir, os.Environ(), input, "valgrind",
		"--quiet", "--leak-check=full", "--show-leak-kinds=all", "--errors-for-leak-kinds=definite,indirect,possible",
		"--error-exitcode=97", "--track-fds=yes", "--trace-children=no", "--suppressions="+supp, o.target)
	text := out.Stderr
	if out.StartError != "" || out.TimedOut {
		return true, "valgrind check did not complete: " + firstNonEmpty(out.StartError, "timed out") + "\n" + text
	}
	return out.ExitCode == 97 || strings.Contains(text, "definitely lost:") && !strings.Contains(text, "definitely lost: 0 bytes"), text
}

func normalize(s, tmp string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = ansiRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, tmp, "<TMP>")
	return strings.TrimSuffix(s, "\n")
}

func normalizeStderr(s, tmp string) string {
	s = normalize(s, tmp)
	s = bashPrefixRE.ReplaceAllString(s, "")
	s = shellPrefixRE.ReplaceAllString(s, "")
	return s
}

func stripInputEcho(output, input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	if strings.HasPrefix(output, input) {
		return strings.TrimPrefix(output[len(input):], "\n")
	}
	return output
}

func sameWhenSorted(a, b string) bool {
	if a == b || a == "" || b == "" {
		return false
	}
	aa, bb := strings.Split(a, "\n"), strings.Split(b, "\n")
	sort.Strings(aa)
	sort.Strings(bb)
	return strings.Join(aa, "\n") == strings.Join(bb, "\n")
}

func versionSensitive(input string, bash runOutput) bool {
	return bash.ExitCode == 2 && containsBuiltin(input)
}

func containsBuiltin(s string) bool {
	for _, b := range []string{"exit", "export", "unset", "cd", "echo", "pwd", "env"} {
		if strings.Contains(" "+strings.ToLower(s), " "+b) {
			return true
		}
	}
	return false
}

func differenceReason(a, b runOutput) string {
	var p []string
	if a.StartError != "" || b.StartError != "" {
		p = append(p, "process start error")
	}
	if a.Stdout != b.Stdout {
		p = append(p, "stdout")
	}
	if a.Stderr != b.Stderr {
		p = append(p, "stderr")
	}
	if a.ExitCode != b.ExitCode {
		p = append(p, fmt.Sprintf("exit code minishell=%d bash=%d", a.ExitCode, b.ExitCode))
	}
	return strings.Join(p, ", ") + " differs"
}

func printResults(rs []result, o options) {
	if o.format == "json" {
		payload := struct {
			Tool             string   `json:"tool"`
			BashVersion      string   `json:"bash_version"`
			CoreutilsVersion string   `json:"coreutils_version"`
			Results          []result `json:"results"`
		}{"slop", o.bashVersion, o.coreutils, rs}
		fatalIf(json.NewEncoder(os.Stdout).Encode(payload))
		return
	}
	if o.format == "jsonl" {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range rs {
			fatalIf(enc.Encode(r))
		}
		return
	}
	pass, fail, warn := 0, 0, 0
	for _, r := range rs {
		switch r.Status {
		case "pass":
			pass++
		case "fail":
			fail++
		case "warning":
			warn++
		}
		if o.failuresOnly && r.Status == "pass" {
			continue
		}
		if r.Status == "pass" {
			fmt.Printf("PASS %s\n", r.ID)
			continue
		}
		printFailure(r)
	}
	if o.versionDrift {
		fmt.Printf("\nWARN host differs from Ubuntu 22.04 baseline: %s | %s\n", firstLine(o.bashVersion), firstLine(o.coreutils))
	}
	fmt.Printf("\nRESULT pass=%d warning=%d fail=%d total=%d\n", pass, warn, fail, len(rs))
}

func printFailure(r result) {
	label := strings.ToUpper(r.Status)
	fmt.Printf("\n%s %s — %s\n", label, r.ID, r.Reason)
	fmt.Printf("input:\n%s", indent(r.Input))
	fmt.Printf("minishell exit=%d stdout:\n%s\nminishell stderr:\n%s\n", r.Minishell.ExitCode, indent(r.Minishell.Stdout), indent(r.Minishell.Stderr))
	fmt.Printf("bash exit=%d stdout:\n%s\nbash stderr:\n%s\n", r.Bash.ExitCode, indent(r.Bash.Stdout), indent(r.Bash.Stderr))
	fmt.Printf("versions: %s | %s\n", firstLine(r.BashVersion), firstLine(r.Coreutils))
	fmt.Printf("verify minishell: %s\nverify bash:      %s\n", r.MinishellCmd, r.BashCmd)
	if r.LeakOutput != "" {
		fmt.Printf("valgrind:\n%s\n", indent(r.LeakOutput))
	}
}

func verifyCommand(shell, input string) string {
	return "tmp=$(mktemp -d); mkdir -p \"$tmp/outfiles\"; (cd \"$tmp\" && printf %s " + shellQuote(input) + " | env LC_ALL=C TERM=dumb " + shell + "); rc=$?; rm -rf \"$tmp\"; exit $rc"
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func indent(s string) string {
	if s == "" {
		return "  <empty>"
	}
	return "  " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  ") + "\n"
}
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
func ensureNewline(s string) string { return strings.TrimRight(s, "\n") + "\n" }
func sanitizeID(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}
func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown error"
}
func testerErrorResult(t testCase, err error) result {
	return result{ID: t.ID, Input: t.Input, Source: t.Source, Status: "fail", Reason: "tester setup error: " + err.Error()}
}
func commandVersion(name, arg string) string {
	out, err := exec.Command(name, arg).CombinedOutput()
	if err != nil {
		return name + ": unavailable"
	}
	return strings.TrimSpace(string(out))
}
func wasFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
func isTerminal(fd uintptr) bool {
	st, err := os.NewFile(fd, "").Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "slop:", err)
		os.Exit(2)
	}
}
