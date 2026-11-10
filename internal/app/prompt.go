package app

// Terminal output and prompts. Prompts only happen on an interactive terminal; in scripts and CI
// every value comes from flags or environment variables.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"

	"golang.org/x/term"
)

var errNoInput = errors.New("no interactive terminal")

type UI struct {
	in          *bufio.Reader
	out         io.Writer
	interactive bool
	color       bool
}

func newUI() *UI {
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	color := term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" &&
		(runtime.GOOS != "windows" || os.Getenv("WT_SESSION") != "")
	return &UI{in: bufio.NewReader(os.Stdin), out: os.Stdout, interactive: interactive, color: color}
}

func (u *UI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (u *UI) bold(s string) string  { return u.paint("1", s) }
func (u *UI) dim(s string) string   { return u.paint("2", s) }
func (u *UI) green(s string) string { return u.paint("32", s) }
func (u *UI) amber(s string) string { return u.paint("33", s) }
func (u *UI) red(s string) string   { return u.paint("31", s) }

func (u *UI) Printf(format string, a ...any) { fmt.Fprintf(u.out, format, a...) }
func (u *UI) Title(s string)                 { u.Printf("\n%s\n", u.bold(s)) }
func (u *UI) Section(s string)               { u.Printf("\n%s\n", u.bold(s)) }
func (u *UI) OK(format string, a ...any) {
	u.Printf("  %s %s\n", u.green("✓"), fmt.Sprintf(format, a...))
}
func (u *UI) Warn(format string, a ...any) {
	u.Printf("  %s %s\n", u.amber("!"), fmt.Sprintf(format, a...))
}
func (u *UI) Info(format string, a ...any) { u.Printf("  %s\n", fmt.Sprintf(format, a...)) }
func (u *UI) Step(n, total int, s string) {
	u.Printf("\n%s %s\n", u.bold(fmt.Sprintf("[%d/%d]", n, total)), s)
}

// Ask reads a visible value. A non-empty def is used when the answer is empty.
func (u *UI) Ask(label, hint, def string) (string, error) {
	if !u.interactive {
		return "", errNoInput
	}
	if hint != "" {
		u.Printf("  %s\n", u.dim(hint))
	}
	if def != "" {
		u.Printf("  %s [%s]: ", label, def)
	} else {
		u.Printf("  %s: ", label)
	}
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = def
	}
	return line, nil
}

// Secret reads a value without echoing it (passwords, keys, connection strings).
func (u *UI) Secret(label, hint string) (string, error) {
	if !u.interactive {
		return "", errNoInput
	}
	if hint != "" {
		u.Printf("  %s\n", u.dim(hint))
	}
	u.Printf("  %s: ", label)
	fd := int(os.Stdin.Fd())
	stop := restoreTerminalOnInterrupt(fd)
	b, err := term.ReadPassword(fd)
	stop()
	u.Printf("\n")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// restoreTerminalOnInterrupt covers Ctrl+C at a hidden prompt. The echo is off while the value is typed; if the
// process just died there, the shell would be left without echo. Prompts only happen before anything is changed.
func restoreTerminalOnInterrupt(fd int) (stop func()) {
	state, err := term.GetState(fd)
	if err != nil {
		return func() {}
	}
	sig, done := make(chan os.Signal, 1), make(chan struct{})
	signal.Notify(sig, os.Interrupt)
	go func() {
		select {
		case <-sig:
			_ = term.Restore(fd, state)
			fmt.Fprintln(os.Stderr, "\n"+cancelledMsg)
			os.Exit(130)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
	}
}

// Confirm asks a yes/no question.
func (u *UI) Confirm(question string, defYes bool) (bool, error) {
	if !u.interactive {
		return false, errNoInput
	}
	opts := "y/N"
	if defYes {
		opts = "Y/n"
	}
	u.Printf("  %s [%s]: ", question, opts)
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "":
		return defYes, nil
	}
	return false, nil
}

// value resolves a setting from a flag, then an environment variable, then a prompt.
func (u *UI) value(flagVal string, envs []string, prompt func() (string, error)) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	for _, e := range envs {
		if v := strings.TrimSpace(os.Getenv(e)); v != "" {
			return v, nil
		}
	}
	if prompt == nil {
		return "", nil
	}
	return prompt()
}
