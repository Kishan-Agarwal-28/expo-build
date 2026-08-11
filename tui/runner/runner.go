// Package runner handles spawning and streaming the Gradle/Expo build process.
// The design mirrors the CLI exactly for command construction (same args, same
// wsl.exe invocation) and avoids all channel-backpressure issues by writing
// output into a mutex-protected SharedLog that the TUI polls on every tick.
package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/kishan-agarwal-28/expo-tui/db"
)

// ─── Shared log ───────────────────────────────────────────────────────────────

// SharedLog is a thread-safe, append-only log buffer.
// The runner goroutine writes to it freely without blocking;
// the TUI model reads from it on every tick.
type SharedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *SharedLog) writeLine(line string) {
	l.mu.Lock()
	l.buf.WriteString(line)
	l.buf.WriteByte('\n')
	l.mu.Unlock()
}

// String returns a snapshot of all log output so far.
func (l *SharedLog) String() string {
	l.mu.Lock()
	s := l.buf.String()
	l.mu.Unlock()
	return s
}

// ─── Public API ───────────────────────────────────────────────────────────────

// BuildOptions is everything needed to run a build.
type BuildOptions struct {
	ProjectDir string
	AppRelDir  string
	BuildType  string // "Debug" | "Production" | "Signing Report"
	Format     string // "APK -- …" | "AAB -- …"
	Settings   db.Settings
	ScriptBody []byte // embedded build.bash content
}

// BuildDoneMsg is sent to bubbletea when the build finishes.
type BuildDoneMsg struct {
	Success      bool
	ArtifactPath string
	Err          string
}

// Start launches the build in a background goroutine. It immediately returns:
//   - *SharedLog — live-updated; safe to read from any goroutine at any time
//   - tea.Cmd   — a Cmd that blocks until the build is done, then returns BuildDoneMsg
//
// The caller should:
//  1. Store the *SharedLog in the model.
//  2. Return the tea.Cmd from Update so bubbletea runs it.
//  3. Poll SharedLog.String() on each tickMsg to refresh the viewport.
func Start(opts BuildOptions) (*SharedLog, tea.Cmd) {
	log := &SharedLog{}
	doneCh := make(chan BuildDoneMsg, 1)

	go func() {
		doneCh <- run(opts, log)
	}()

	// WaitCmd blocks (off the main goroutine, inside bubbletea's Cmd pool)
	// until the goroutine sends its result.
	waitCmd := func() tea.Msg {
		return <-doneCh
	}
	return log, waitCmd
}

// ─── Core build logic ─────────────────────────────────────────────────────────

func run(opts BuildOptions, log *SharedLog) BuildDoneMsg {
	// 1. Write embedded script to a temp file (strip Windows line endings).
	tmp, err := os.CreateTemp("", "expo-build-*.bash")
	if err != nil {
		return BuildDoneMsg{Err: "temp script: " + err.Error()}
	}
	clean := bytes.ReplaceAll(opts.ScriptBody, []byte("\r\n"), []byte("\n"))
	if _, err := tmp.Write(clean); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return BuildDoneMsg{Err: "write script: " + err.Error()}
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	// 2. Build the command — identical construction to the CLI.
	cmd, err := buildCmd(tmp.Name(), opts)
	if err != nil {
		return BuildDoneMsg{Err: err.Error()}
	}

	// 3. Use a real OS pipe so the subprocess writes directly to the kernel
	//    pipe buffer (no Go-level copying goroutine, no backpressure).
	pr, pw, err := os.Pipe()
	if err != nil {
		return BuildDoneMsg{Err: "pipe: " + err.Error()}
	}

	cmd.Stdout = pw
	cmd.Stderr = pw
	// Stdin: bubbletea owns the terminal so we can't forward it.
	// The build.bash script uses "yes |" for interactive SDK acceptance,
	// so nil stdin is safe.
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return BuildDoneMsg{Err: "start process: " + err.Error()}
	}

	// Close the parent's write-end after handing it to the child.
	// The child has inherited its own copy via fork/CreateProcess; once the
	// child exits its copy is closed and the reader sees EOF.
	pw.Close()

	// 4. Stream output into SharedLog — no channel, no backpressure.
	scanner := bufio.NewScanner(pr)
	// Gradle --info lines can be very long; bump the buffer.
	scanner.Buffer(make([]byte, 512*1024), 512*1024)
	for scanner.Scan() {
		log.writeLine(scanner.Text())
	}
	if scanErr := scanner.Err(); scanErr != nil {
		log.writeLine("[runner] scan output: " + scanErr.Error())
	}
	pr.Close()

	// 5. Reap the process and collect the exit code.
	waitErr := cmd.Wait()

	success := waitErr == nil
	artifactPath := ""
	if success {
		artifactPath = deriveArtifact(opts)
		if artifactPath != "" {
			if _, statErr := os.Stat(artifactPath); statErr != nil {
				// File not found — treat as failure (e.g. Signing Report).
				artifactPath = ""
			}
		}
	}

	if !success {
		log.writeLine("[runner] process exited: " + waitErr.Error())
	}

	return BuildDoneMsg{
		Success:      success,
		ArtifactPath: artifactPath,
	}
}

// buildCmd constructs the exec.Cmd exactly as the CLI does.
func buildCmd(scriptPath string, opts BuildOptions) (*exec.Cmd, error) {
	if runtime.GOOS == "windows" {
		wslScript, err := toWSLPath(scriptPath)
		if err != nil {
			return nil, fmt.Errorf("wslpath (script): %w", err)
		}
		wslProject, err := toWSLPath(opts.ProjectDir)
		if err != nil {
			return nil, fmt.Errorf("wslpath (project): %w", err)
		}
		wslRel := filepath.ToSlash(opts.AppRelDir)

		// Matches the CLI's runBuildInWSL exactly.
		shellCmd := fmt.Sprintf("bash %s %s %s %s %s",
			shellQuote(wslScript),
			shellQuote(wslProject),
			shellQuote(opts.BuildType),
			shellQuote(wslRel),
			shellQuote(opts.Format),
		)
		return exec.Command("wsl.exe", "bash", "-lic", shellCmd), nil
	}

	// Non-Windows: identical to runBuildDirect in the CLI.
	return exec.Command("bash", scriptPath,
		opts.ProjectDir,
		opts.BuildType,
		filepath.ToSlash(opts.AppRelDir),
		opts.Format,
	), nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func deriveArtifact(opts BuildOptions) string {
	if opts.BuildType == "Signing Report" {
		return ""
	}
	appDir := filepath.Join(opts.ProjectDir, opts.AppRelDir)
	isAAB := strings.HasPrefix(opts.Format, "AAB")
	isProd := opts.BuildType == "Production"

	if isAAB {
		if isProd {
			return filepath.Join(appDir, "app-release.aab")
		}
		return filepath.Join(appDir, "app-debug.aab")
	}
	if isProd {
		return filepath.Join(appDir, "app-release.apk")
	}
	return filepath.Join(appDir, "app-debug.apk")
}

func toWSLPath(winPath string) (string, error) {
	safe := filepath.ToSlash(winPath)
	out, err := exec.Command("wsl.exe", "wslpath", "-a", safe).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
