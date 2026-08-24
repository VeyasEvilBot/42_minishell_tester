package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

type ptyAction struct {
	Data  string
	Delay time.Duration
}

type ptyScenario struct {
	Description string
	Actions     []ptyAction
	WantStatus  []int
	NeedAlive   bool
	History     bool
}

type ptyResult struct {
	Transcript string
	Statuses   []int
	ExitCode   int
	TimedOut   bool
	Err        error
}

var statusRE = regexp.MustCompile(`__SLOP_STATUS__([0-9]+)`)
var valgrindErrorRE = regexp.MustCompile(`ERROR SUMMARY: [1-9][0-9]* errors`)

func interactiveTests() []testCase {
	ids := []string{
		"readline.ctrl_c_prompt",
		"readline.ctrl_c_partial_line",
		"readline.ctrl_c_child",
		"readline.ctrl_backslash_child",
		"readline.ctrl_c_heredoc",
		"readline.ctrl_c_multiple_heredocs",
		"readline.ctrl_d_heredoc",
		"readline.ctrl_d_prompt",
		"readline.ctrl_c_then_ctrl_d",
		"readline.history_up",
		"readline.ctrl_l_redraw",
	}
	out := make([]testCase, 0, len(ids))
	for _, id := range ids {
		s := ptyScenarios()[id]
		out = append(out, testCase{ID: id, Input: s.Description, Source: "interactive/pty", Scenario: id})
	}
	return out
}

func ptyScenarios() map[string]ptyScenario {
	key := func(data string) ptyAction { return ptyAction{Data: data, Delay: 140 * time.Millisecond} }
	status := key("printf '__SLOP_STATUS__%d\\n' \"$?\"\n")
	alive := key("printf '__SLOP_ALIVE__\\n'\n")
	exit := key("exit\n")
	return map[string]ptyScenario{
		"readline.ctrl_c_prompt": {
			Description: "PTY: Ctrl-C at an empty Readline prompt; shell must survive and set status 130",
			Actions:     []ptyAction{key("\x03"), status, alive, exit}, WantStatus: []int{130}, NeedAlive: true,
		},
		"readline.ctrl_c_partial_line": {
			Description: "PTY: type a partial command, Ctrl-C it, then prove the shell is alive",
			Actions:     []ptyAction{key("echo __SLOP_SHOULD_NOT_RUN__"), key("\x03"), status, alive, exit}, WantStatus: []int{130}, NeedAlive: true,
		},
		"readline.ctrl_c_child": {
			Description: "PTY: interrupt foreground cat with Ctrl-C; shell must survive with status 130",
			Actions:     []ptyAction{key("cat\n"), key("\x03"), status, alive, exit}, WantStatus: []int{130}, NeedAlive: true,
		},
		"readline.ctrl_backslash_child": {
			Description: "PTY: interrupt foreground cat with Ctrl-\\; shell must survive with status 131",
			Actions:     []ptyAction{key("cat\n"), key("\x1c"), status, alive, exit}, WantStatus: []int{131}, NeedAlive: true,
		},
		"readline.ctrl_c_heredoc": {
			Description: "PTY: interrupt a heredoc with Ctrl-C; shell must survive with status 130",
			Actions:     []ptyAction{key("cat << EOF\n"), key("body\n"), key("\x03"), status, alive, exit}, WantStatus: []int{130}, NeedAlive: true,
		},
		"readline.ctrl_c_multiple_heredocs": {
			Description: "PTY: interrupt multiple heredocs with one Ctrl-C; shell must survive",
			Actions:     []ptyAction{key("cat << A << B\n"), key("first\n"), key("\x03"), status, alive, exit}, WantStatus: []int{130}, NeedAlive: true,
		},
		"readline.ctrl_d_heredoc": {
			Description: "PTY: Ctrl-D before a heredoc delimiter; shell must recover like Bash",
			Actions:     []ptyAction{key("cat << EOF\n"), key("body\n"), key("\x04"), status, alive, exit}, WantStatus: []int{0}, NeedAlive: true,
		},
		"readline.ctrl_d_prompt": {
			Description: "PTY: Ctrl-D at an empty prompt exits cleanly",
			Actions:     []ptyAction{key("\x04")},
		},
		"readline.ctrl_c_then_ctrl_d": {
			Description: "PTY: Ctrl-C immediately followed by Ctrl-D preserves Bash-compatible exit status",
			Actions:     []ptyAction{key("\x03"), key("\x04")},
		},
		"readline.history_up": {
			Description: "PTY: execute a command, recall it with Up, and execute it again",
			Actions:     []ptyAction{key("printf '__SLOP_HISTORY_OUTPUT__\\n'\n"), key("\x1b[A"), key("\n"), exit}, History: true,
		},
		"readline.ctrl_l_redraw": {
			Description: "PTY: Ctrl-L redraw does not lose or kill the prompt",
			Actions:     []ptyAction{key("\x0c"), alive, exit}, NeedAlive: true,
		},
	}
}

func runInteractiveOne(t testCase, o options) result {
	start := time.Now()
	s := ptyScenarios()[t.Scenario]
	var mini, bash ptyResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); mini = runPTY(o.timeout, o.target, nil, s) }()
	go func() {
		defer wg.Done()
		bash = runPTY(o.timeout, "bash", []string{"--noprofile", "--norc", "--posix", "-i"}, s)
	}()
	wg.Wait()

	r := result{
		ID: t.ID, Input: t.Input, Source: t.Source,
		Minishell:      runOutput{Stdout: mini.Transcript, ExitCode: mini.ExitCode, TimedOut: mini.TimedOut},
		Bash:           runOutput{Stdout: bash.Transcript, ExitCode: bash.ExitCode, TimedOut: bash.TimedOut},
		DurationMillis: time.Since(start).Milliseconds(),
	}
	r.Status, r.Reason = judgePTY(s, mini, bash)
	if r.Status != "pass" {
		r.BashVersion, r.Coreutils = o.bashVersion, o.coreutils
		r.MinishellCmd = "interactive PTY scenario: " + s.Description
		r.BashCmd = "bash --noprofile --norc --posix -i  # repeat the same keys in a real terminal"
	}
	if o.leaks {
		leaked, transcript := runPTYLeak(o, s)
		if leaked {
			r.Status = "fail"
			r.Reason = joinReason(r.Reason, "valgrind reported errors during PTY scenario")
			r.LeakOutput = transcript
		}
	}
	r.DurationMillis = time.Since(start).Milliseconds()
	return r
}

func runPTY(timeout time.Duration, name string, args []string, s ptyScenario) ptyResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "PS1=SLOP> ", "PS2=MORE> ", "TERM=xterm", "LC_ALL=C")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 120})
	if err != nil {
		return ptyResult{ExitCode: 127, Err: err}
	}
	var buf bytes.Buffer
	copyDone := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, ptmx); close(copyDone) }()
	time.Sleep(180 * time.Millisecond)
	for _, action := range s.Actions {
		if action.Delay > 0 {
			time.Sleep(action.Delay)
		}
		_, _ = ptmx.Write([]byte(action.Data))
	}
	waitErr := cmd.Wait()
	_ = ptmx.Close()
	select {
	case <-copyDone:
	case <-time.After(300 * time.Millisecond):
	}
	code := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
		} else {
			code = 127
		}
	}
	if ctx.Err() == context.DeadlineExceeded {
		code = 124
	}
	transcript := ansiRE.ReplaceAllString(strings.ReplaceAll(buf.String(), "\r", ""), "")
	matches := statusRE.FindAllStringSubmatch(transcript, -1)
	statuses := make([]int, 0, len(matches))
	for _, m := range matches {
		n, _ := strconv.Atoi(m[1])
		statuses = append(statuses, n)
	}
	return ptyResult{Transcript: strings.TrimSpace(transcript), Statuses: statuses, ExitCode: code, TimedOut: ctx.Err() == context.DeadlineExceeded, Err: waitErr}
}

func judgePTY(s ptyScenario, mini, bash ptyResult) (string, string) {
	if mini.TimedOut {
		return "fail", "minishell PTY scenario timed out"
	}
	if bash.TimedOut {
		return "warning", "bash PTY scenario timed out; host is unreliable"
	}
	if len(s.WantStatus) > 0 {
		if !equalInts(bash.Statuses, s.WantStatus) {
			return "warning", fmt.Sprintf("bash returned marker statuses %v, expected %v; host behavior differs", bash.Statuses, s.WantStatus)
		}
		if !equalInts(mini.Statuses, bash.Statuses) {
			return "fail", fmt.Sprintf("PTY status markers differ: minishell=%v bash=%v", mini.Statuses, bash.Statuses)
		}
	}
	if s.NeedAlive {
		if !strings.Contains(bash.Transcript, "__SLOP_ALIVE__") {
			return "warning", "bash did not reach the alive marker"
		}
		if !strings.Contains(mini.Transcript, "__SLOP_ALIVE__") {
			return "fail", "minishell did not recover to execute the alive marker"
		}
	}
	if s.History {
		bc := strings.Count(bash.Transcript, "__SLOP_HISTORY_OUTPUT__")
		mc := strings.Count(mini.Transcript, "__SLOP_HISTORY_OUTPUT__")
		if bc < 4 {
			return "warning", fmt.Sprintf("bash history transcript was unexpected (marker count %d)", bc)
		}
		if mc != bc {
			return "fail", fmt.Sprintf("Readline history recall differs: minishell marker count=%d bash=%d", mc, bc)
		}
	}
	if len(s.WantStatus) == 0 && !s.NeedAlive && !s.History && mini.ExitCode != bash.ExitCode {
		return "fail", fmt.Sprintf("interactive exit differs: minishell=%d bash=%d", mini.ExitCode, bash.ExitCode)
	}
	return "pass", ""
}

func runPTYLeak(o options, s ptyScenario) (bool, string) {
	dir, err := os.MkdirTemp("", "slop-pty-leak-*")
	if err != nil {
		return true, err.Error()
	}
	defer os.RemoveAll(dir)
	supp := filepath.Join(dir, "minishell.supp")
	b, _ := corpus.ReadFile("corpus/minishell.supp")
	_ = os.WriteFile(supp, b, 0600)
	args := []string{
		"--leak-check=full", "--show-leak-kinds=all", "--errors-for-leak-kinds=definite,indirect,possible",
		"--error-exitcode=97", "--track-fds=yes", "--trace-children=no", "--suppressions=" + supp, o.target,
	}
	out := runPTY(o.timeout*4, "valgrind", args, s)
	leaked := valgrindErrorRE.MatchString(out.Transcript) || strings.Contains(out.Transcript, "definitely lost:") && !strings.Contains(out.Transcript, "definitely lost: 0 bytes")
	return leaked, out.Transcript
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
