// Package tracker provides git-based version control for built APK/AAB artifacts.
// One branch per appID is created in ~/.expo-build/track/.
package tracker

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TrackDir returns the path to the tracking git repository.
func TrackDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".expo-build", "track"), nil
}

// EnsureRepo creates the tracking repository if it doesn't exist yet.
func EnsureRepo() (string, error) {
	dir, err := TrackDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create track dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if err := runGit(dir, "init"); err != nil {
			return "", fmt.Errorf("git init: %w", err)
		}
		// Set a local identity so commits never fail due to missing config.
		_ = runGit(dir, "config", "user.email", "expo-build@local")
		_ = runGit(dir, "config", "user.name", "expo-build-tracker")

		readme := filepath.Join(dir, "README.md")
		_ = os.WriteFile(readme, []byte("# expo-build APK tracker\n"), 0644)
		_ = runGit(dir, "add", "README.md")
		_ = runGit(dir, "commit", "-m", "init: create tracking repo")
	}
	return dir, nil
}

// CommitArtifact zips the artifact and commits it to the branch for appID.
// It always returns to the default branch after committing so the repo is
// in a clean state for the next call.
func CommitArtifact(appID, buildID, artifactPath string, logFn func(string)) error {
	dir, err := EnsureRepo()
	if err != nil {
		return err
	}

	branchName := sanitizeBranch(appID)

	// --- always start from the default branch (avoids dirty-tree conflicts) ---
	defaultBr := getDefaultBranch(dir)
	// Stash any uncommitted changes before switching (defensive).
	_ = runGit(dir, "stash")
	_ = runGit(dir, "checkout", defaultBr)

	// --- switch to (or create) the app branch ---
	if branchExists(dir, branchName) {
		if err := runGit(dir, "checkout", branchName); err != nil {
			return fmt.Errorf("checkout %s: %w", branchName, err)
		}
	} else {
		if err := runGit(dir, "checkout", "-b", branchName); err != nil {
			return fmt.Errorf("create branch %s: %w", branchName, err)
		}
	}

	// --- zip & commit ---
	zipName := buildID + ".zip"
	zipPath := filepath.Join(dir, zipName)
	logFn(fmt.Sprintf("[tracker] Compressing artifact -> %s\n", zipPath))
	if err := zipFile(artifactPath, zipPath); err != nil {
		_ = runGit(dir, "checkout", defaultBr)
		return fmt.Errorf("zip: %w", err)
	}

	logFn("[tracker] Committing to git...\n")
	if err := runGit(dir, "add", zipName); err != nil {
		_ = runGit(dir, "checkout", defaultBr)
		return fmt.Errorf("git add: %w", err)
	}
	msg := fmt.Sprintf("build: %s at %s", buildID, time.Now().Format(time.RFC3339))
	if err := runGit(dir, "commit", "-m", msg); err != nil {
		_ = runGit(dir, "checkout", defaultBr)
		return fmt.Errorf("git commit: %w", err)
	}

	logFn(fmt.Sprintf("[tracker] APK committed to branch '%s'\n", branchName))

	// --- return to default branch so the working tree stays clean ---
	_ = runGit(dir, "checkout", defaultBr)
	return nil
}

// RestoreArtifact extracts the zip for buildID from the appID branch and
// writes it into destDir/<buildID>/.  Returns the output directory path.
func RestoreArtifact(appID, buildID, destDir string) (string, error) {
	dir, err := TrackDir()
	if err != nil {
		return "", err
	}
	branch := sanitizeBranch(appID)
	zipName := buildID + ".zip"

	// git-show emits raw binary for blob objects — capture it.
	cmd := exec.Command("git", "show", branch+":"+zipName)
	cmd.Dir = dir
	zipData, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("artifact not in tracking (branch %s, file %s): %w",
			branch, zipName, err)
	}

	outDir := filepath.Join(destDir, buildID)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	zipPath := filepath.Join(outDir, zipName)
	if err := os.WriteFile(zipPath, zipData, 0644); err != nil {
		return "", fmt.Errorf("write zip: %w", err)
	}

	if err := unzipAll(zipPath, outDir); err != nil {
		return "", fmt.Errorf("unzip: %w", err)
	}
	return outDir, nil
}

// ListBranchArtifacts returns all zip filenames committed for an appID.
func ListBranchArtifacts(appID string) ([]string, error) {
	dir, err := TrackDir()
	if err != nil {
		return nil, err
	}
	branch := sanitizeBranch(appID)
	out, err := gitOutput(dir, "ls-tree", "--name-only", branch)
	if err != nil {
		return nil, nil // branch may not exist yet
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(line, ".zip") {
			files = append(files, line)
		}
	}
	return files, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func sanitizeBranch(s string) string {
	r := strings.NewReplacer("/", "-", "\\", "-", " ", "-", ":", "-")
	return r.Replace(s)
}

func branchExists(dir, branch string) bool {
	out, _ := gitOutput(dir, "branch", "--list", branch)
	return strings.TrimSpace(out) != ""
}

// getDefaultBranch returns the current HEAD branch name (usually "main" or "master").
func getDefaultBranch(dir string) string {
	out, err := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "main"
	}
	b := strings.TrimSpace(out)
	if b == "" || b == "HEAD" {
		return "main"
	}
	return b
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func zipFile(src, dst string) error {
	zf, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer zf.Close()

	w := zip.NewWriter(zf)
	defer w.Close()

	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Method = zip.Deflate
	fw, err := w.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, f)
	return err
}

func unzipAll(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		dest := filepath.Join(destDir, f.Name)
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(dest, 0755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(dest), 0755)
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
