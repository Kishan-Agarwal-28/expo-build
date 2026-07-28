package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/mdp/qrterminal/v3"
	"github.com/schollz/progressbar/v3"
)

//go:embed build.bash
var buildScript []byte

var (
	S3BucketName string
	AWSEndpoint  string
	AWSAccessKey string
	AWSSecretKey string
	AWSRegion    string
	BASE_URL     string
)

var app_id string
var qrlink string

func main() {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Println("Error getting current working directory:", err)
		panic(err)
	}

	// appRelDir is the path (relative to dir) of the actual Expo project.
	// For a plain (non-monorepo) project this is ".". For a monorepo/turborepo
	// it's whichever sub-package actually contains the Expo app.
	appRelDir := resolveAppDir(dir)
	appAbsDir := filepath.Join(dir, appRelDir)

	app_id = checkAppID(appAbsDir)

	// --- INTERACTIVE PROMPTS ---
	var action string
	actionPrompt := &survey.Select{
		Message: "What would you like to do?",
		Options: []string{"Build a new APK", "Upload an existing APK"},
	}
	survey.AskOne(actionPrompt, &action)

	if action == "Upload an existing APK" {
		var apkPath string
		pathPrompt := &survey.Input{
			Message: "Path to the existing APK:",
			Default: filepath.Join(appAbsDir, "app-debug.apk"),
		}
		survey.AskOne(pathPrompt, &apkPath)

		deliveryMethod := askDeliveryMethod()
		handleDeliveryAndExit(apkPath, deliveryMethod)
		return
	}

	var buildType string
	typePrompt := &survey.Select{
		Message: "Select the build type:",
		Options: []string{"Debug", "Production", "Signing Report"},
	}
	survey.AskOne(typePrompt, &buildType)

	// Build format only applies when producing an actual artifact.
	buildFormat := ""
	if buildType != "Signing Report" {
		survey.AskOne(&survey.Select{
			Message: "Select the output format:",
			Options: []string{
				"APK — sideload directly onto a device",
				"AAB — upload to Play Store",
			},
		}, &buildFormat)
	}

	// Ask delivery method before the build so the user is not left waiting
	// for input after a potentially long compile.
	// AAB files cannot be sideloaded, so local/S3 transfer is only for APK.
	// Signing Report produces no artifact, so skip entirely.
	deliveryMethod := ""
	if buildType != "Signing Report" {
		if strings.HasPrefix(buildFormat, "APK") {
			deliveryMethod = askDeliveryMethod()
		} else {
			fmt.Println("AAB selected — artifact will be saved locally for Play Store upload.")
		}
	}
	// ---------------------------

	scriptPath, err := createTempScript()
	if err != nil {
		fmt.Println("Error extracting embedded script:", err)
		panic(err)
	}
	defer os.Remove(scriptPath)

	if runtime.GOOS != "windows" {
		if err := runBuildDirect(scriptPath, dir, buildType, appRelDir, buildFormat); err != nil {
			panic(err)
		}
	} else {
		fmt.Println("Windows detected... checking if WSL is installed")
		if !checkWSL() {
			fmt.Println("WSL is not installed")
			fmt.Println("Installing WSL (this requires admin rights)...")
			if !installWSL() {
				panic("Error installing WSL")
			}
			fmt.Println("WSL installed. A reboot is usually required. Please reboot and re-run.")
			return
		}
		if err := runBuildInWSL(scriptPath, dir, buildType, appRelDir, buildFormat); err != nil {
			panic(err)
		}
	}

	fmt.Println("Build phase finished.")

	if buildType == "Signing Report" {
		fmt.Println("Signing report generated successfully. No artifact to deliver.")
		return
	}

	// Derive the output filename from build type + format.
	isAAB := strings.HasPrefix(buildFormat, "AAB")
	var artifactName string
	if isAAB {
		if buildType == "Production" {
			artifactName = "app-release.aab"
		} else {
			artifactName = "app-debug.aab"
		}
	} else {
		if buildType == "Production" {
			artifactName = "app-release.apk"
		} else {
			artifactName = "app-debug.apk"
		}
	}

	artifactPath := filepath.Join(appAbsDir, artifactName)

	if isAAB {
		fmt.Printf("AAB ready at: %s\nUpload it to the Play Console to distribute.\n", artifactPath)
		return
	}

	handleDeliveryAndExit(artifactPath, deliveryMethod)
}

const (
	deliveryLocal = "Local network (WiFi) — instant, no internet needed"
	deliveryS3    = "Upload to S3 — share link across the internet"
)

// askDeliveryMethod prompts the user to choose how the APK should be
// delivered to the device and returns their selection.
func askDeliveryMethod() string {
	var choice string
	survey.AskOne(&survey.Select{
		Message: "How would you like to deliver the APK?",
		Options: []string{deliveryLocal, deliveryS3},
	}, &choice)
	return choice
}

// handleDeliveryAndExit routes to the delivery method the user already chose
// (via askDeliveryMethod) before the build started.
func handleDeliveryAndExit(apkPath, deliveryMethod string) {
	if deliveryMethod == deliveryLocal {
		if err := serveAPKLocally(apkPath); err != nil {
			fmt.Println("Local transfer failed:", err)
		}
		return
	}

	// S3 path (original behaviour)
	if S3BucketName == "" {
		fmt.Println("Warning: S3BucketName was not injected during the build process!")
	}
	if err := uploadAPKToS3(apkPath, S3BucketName); err != nil {
		fmt.Println("Error uploading to S3:", err)
		return
	}
	if qrlink == "" {
		panic("Error generating download link")
	}
	fmt.Printf("APK uploaded successfully. Download the app using the following link:\n  %s\nOR scan the QR code below:\n\n", qrlink)
	qrterminal.GenerateHalfBlock(qrlink, qrterminal.M, os.Stdout)
}

// serveAPKLocally starts a temporary HTTP server on the local network,
// prints (and QR-encodes) the URL, waits for the phone to download the
// APK, then shuts down automatically.
//
// Both the dev machine and the phone must be on the same WiFi/LAN.
// No internet connection is required for either device.
func serveAPKLocally(apkPath string) error {
	// Verify the APK exists before binding a port.
	info, err := os.Stat(apkPath)
	if err != nil {
		return fmt.Errorf("APK not found at %s: %w", apkPath, err)
	}

	localIP, err := getOutboundLocalIP()
	if err != nil {
		return fmt.Errorf("could not determine local IP address: %w", err)
	}

	// Bind on an OS-assigned free port so we never collide with another service.
	listener, err := net.Listen("tcp", localIP+":0")
	if err != nil {
		return fmt.Errorf("could not bind local port: %w", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	filename := filepath.Base(apkPath)
	url := fmt.Sprintf("http://%s:%d/%s", localIP, port, filename)

	// done is closed by the handler after one full successful download,
	// which causes the server goroutine to shut down.
	done := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/"+filename, func(w http.ResponseWriter, r *http.Request) {
		// Open the APK fresh for every request so Content-Length is accurate
		// and concurrent/retry requests each get their own file descriptor.
		f, ferr := os.Open(apkPath)
		if ferr != nil {
			http.Error(w, "APK unavailable", http.StatusInternalServerError)
			return
		}
		defer f.Close()

		w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))

		bar := progressbar.DefaultBytes(info.Size(), fmt.Sprintf("Sending %s", filename))
		http.ServeContent(w, r, filename, time.Time{}, &progressReaderSeeker{f: f, bar: bar})

		// Signal shutdown only after the response body has been written.
		// ServeContent has returned, so the download is complete.
		once.Do(func() { close(done) })
	})

	// Graceful shutdown: block until the download finishes or the user Ctrl-Cs.
	srv := &http.Server{Handler: mux}
	go func() {
		<-done
		// Give the TCP stack a moment to flush the last bytes to the phone
		// before we tear down the listener.
		time.Sleep(500 * time.Millisecond)
		srv.Shutdown(context.Background())
	}()

	fmt.Printf("\nAPK ready on local network.\nMake sure your phone is on the same WiFi, then:\n  • Open: %s\n  • OR scan the QR code below:\n\n", url)
	qrterminal.GenerateHalfBlock(url, qrterminal.M, os.Stdout)
	fmt.Println("\nWaiting for download... (Ctrl-C to cancel)")

	// Serve blocks until Shutdown() is called by the goroutine above.
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server error: %w", err)
	}

	fmt.Println("\nTransfer complete! APK delivered to device.")
	return nil
}

// getOutboundLocalIP returns the IP address of the network interface the OS
// would use to reach the internet. We use a UDP "connect" (which never
// actually sends a packet) as a trick to ask the OS to pick the right
// interface for us — giving us the LAN IP rather than 127.0.0.1.
func getOutboundLocalIP() (string, error) {
	// The destination doesn't need to be reachable; UDP connect is instant.
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// progressReaderSeeker wraps an *os.File so that reads are tracked by a
// progressbar while still satisfying io.ReadSeeker (required by
// http.ServeContent for range-request support).
type progressReaderSeeker struct {
	f   *os.File
	bar *progressbar.ProgressBar
}

func (p *progressReaderSeeker) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	p.bar.Add(n)
	return n, err
}

func (p *progressReaderSeeker) Seek(offset int64, whence int) (int64, error) {
	return p.f.Seek(offset, whence)
}

// resolveAppDir figures out which directory (relative to root) actually
// contains the Expo project to build. This makes the tool work whether it's
// run at the root of a single Expo app, or at the root of a monorepo/
// turborepo containing one or more Expo apps nested under it.
func resolveAppDir(root string) string {
	candidates := findExpoProjectDirs(root)

	if len(candidates) == 0 {
		// No app.json/app.config with an "expo" section found anywhere.
		// Fall back to the old behavior and assume the current directory
		// itself is the project (keeps single-repo usage unchanged).
		return "."
	}

	rels := make([]string, len(candidates))
	for i, c := range candidates {
		rel, err := filepath.Rel(root, c)
		if err != nil || rel == "" {
			rel = "."
		}
		rels[i] = rel
	}
	sort.Strings(rels)

	if len(rels) == 1 {
		if rels[0] != "." {
			fmt.Printf("Detected Expo project in monorepo: %s\n", rels[0])
		}
		return rels[0]
	}

	fmt.Println("Multiple Expo projects detected (monorepo/turborepo).")
	var choice string
	projPrompt := &survey.Select{
		Message: "Which project would you like to build?",
		Options: rels,
	}
	survey.AskOne(projPrompt, &choice)
	if choice == "" {
		choice = rels[0]
	}
	return choice
}

// findExpoProjectDirs walks the directory tree starting at root and returns
// the absolute paths of every directory that looks like the root of an Expo
// project. It skips node_modules, VCS folders, native build output, and
// other directories that would never contain a project root.
func findExpoProjectDirs(root string) []string {
	skipDirNames := map[string]bool{
		"node_modules": true,
		"android":      true,
		"ios":          true,
		"build":        true,
		"dist":         true,
		"Pods":         true,
	}

	found := map[string]bool{}

	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entry (permissions, broken symlink, etc) - skip it.
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (skipDirNames[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}

		switch d.Name() {
		case "app.json":
			if isExpoAppJSON(path) {
				found[filepath.Dir(path)] = true
			}
		case "app.config.js", "app.config.ts", "app.config.cjs", "app.config.mjs":
			// Only count it as a project root if a package.json sits
			// alongside it, to avoid matching stray config fragments.
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(path), "package.json")); statErr == nil {
				found[filepath.Dir(path)] = true
			}
		}
		return nil
	})

	dirs := make([]string, 0, len(found))
	for d := range found {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// isExpoAppJSON reports whether the app.json at path has a top-level "expo"
// key, which every real Expo app.json has.
func isExpoAppJSON(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var parsed struct {
		Expo json.RawMessage `json:"expo"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return false
	}
	return len(parsed.Expo) > 0
}

func checkAppID(dir string) string {
	tomlPath := filepath.Join(dir, "expo-build.toml")

	if _, err := os.Stat(tomlPath); errors.Is(err, fs.ErrNotExist) {
		id := uuid.NewString()
		os.WriteFile(tomlPath, []byte("[app]\napp_id = \""+id+"\"\n"), 0644)
		return id
	}

	data, err := os.ReadFile(tomlPath)
	if err != nil {
		fmt.Println("Error reading expo-build.toml:", err)
		id := uuid.NewString()
		os.WriteFile(tomlPath, []byte("[app]\napp_id = \""+id+"\"\n"), 0644)
		return id
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "app_id") {
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				return strings.Trim(strings.TrimSpace(parts[1]), "\"")
			}
		}
	}
	id := uuid.NewString()
	os.WriteFile(tomlPath, []byte("[app]\napp_id = \""+id+"\"\n"), 0644)
	return id
}

func uploadAPKToS3(filePath string, bucketName string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("could not open APK file: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("could not read file details: %w", err)
	}

	if AWSAccessKey == "" || AWSSecretKey == "" || AWSRegion == "" || AWSEndpoint == "" {
		return fmt.Errorf("missing AWS credentials. They were not injected during build")
	}

	staticCreds := credentials.NewStaticCredentialsProvider(AWSAccessKey, AWSSecretKey, "")

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(AWSRegion),
		config.WithCredentialsProvider(staticCreds),
		config.WithBaseEndpoint(AWSEndpoint),
	)
	if err != nil {
		return fmt.Errorf("could not configure AWS: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	filename := filepath.Base(filePath)
	fileExt := filepath.Ext(filePath)
	filenameWithoutExt := strings.TrimSuffix(filename, fileExt)
	timestamp := time.Now().Unix()

	objectKey := app_id + "/" + filenameWithoutExt + "_" + strconv.FormatInt(timestamp, 10) + fileExt
	fmt.Printf("Streaming %s to S3 bucket %s...\n", objectKey, bucketName)

	bar := progressbar.DefaultBytes(
		fileInfo.Size(),
		fmt.Sprintf("Uploading %s", filename),
	)
	proxyReader := progressbar.NewReader(file, bar)

	tm := transfermanager.New(client, func(o *transfermanager.Options) {
		o.PartSizeBytes = 5 * 1024 * 1024
		o.Concurrency = 5
	})

	_, err = tm.UploadObject(context.TODO(), &transfermanager.UploadObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(objectKey),
		Body:   &proxyReader,
	})

	if err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	fmt.Println("\nSuccessfully uploaded to S3!")

	qrlink = fmt.Sprintf("%s?q=%s", BASE_URL, objectKey)
	return nil
}

func createTempScript() (string, error) {
	tmpFile, err := os.CreateTemp("", "build-*.bash")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	cleanScript := bytes.ReplaceAll(buildScript, []byte("\r\n"), []byte("\n"))

	if _, err := tmpFile.Write(cleanScript); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write to temp file: %w", err)
	}

	tmpFile.Close()
	return tmpFile.Name(), nil
}

func checkWSL() bool {
	cmd := exec.Command("wsl.exe", "--status")
	return cmd.Run() == nil
}

func installWSL() bool {
	cmd := exec.Command("wsl.exe", "--install")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run() == nil
}

func toWSLPath(winPath string) (string, error) {
	safePath := filepath.ToSlash(winPath)
	cmd := exec.Command("wsl.exe", "wslpath", "-a", safePath)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("wslpath failed with output: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("wslpath failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func runBuildInWSL(scriptPath, projectDir, buildType, appRelDir, buildFormat string) error {
	wslScriptPath, err := toWSLPath(scriptPath)
	if err != nil {
		return fmt.Errorf("translating script path: %w", err)
	}
	wslProjectPath, err := toWSLPath(projectDir)
	if err != nil {
		return fmt.Errorf("translating project path: %w", err)
	}

	// appRelDir is a relative path; normalize to forward slashes for bash
	// regardless of whether it was computed with Windows separators.
	wslAppRelDir := filepath.ToSlash(appRelDir)

	shellCmd := fmt.Sprintf("bash %s %s %s %s %s",
		shellQuote(wslScriptPath),
		shellQuote(wslProjectPath),
		shellQuote(buildType),
		shellQuote(wslAppRelDir),
		shellQuote(buildFormat),
	)

	cmd := exec.Command("wsl.exe", "bash", "-lic", shellCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runBuildDirect(scriptPath, projectDir, buildType, appRelDir, buildFormat string) error {
	cmd := exec.Command("bash", scriptPath, projectDir, buildType, filepath.ToSlash(appRelDir), buildFormat)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}