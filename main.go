package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

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
)
var app_id string
var qrlink string

func main() {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Println("Error getting current working directory:", err)
		panic(err)
	}
	app_id = checkAppID(dir)
	scriptPath, err := createTempScript()
	if err != nil {
		fmt.Println("Error extracting embedded script:", err)
		panic(err)
	}
	defer os.Remove(scriptPath)

	if runtime.GOOS != "windows" {
		if err := runBuildDirect(scriptPath, dir); err != nil {
			panic(err)
		}
		fmt.Println("Build finished successfully.")
		return
	}

	fmt.Println("Windows detected... checking if WSL is installed")
	if checkWSL() {
		fmt.Println("WSL is installed")
	} else {
		fmt.Println("WSL is not installed")
		fmt.Println("Installing WSL (this requires admin rights)...")
		if !installWSL() {
			panic("Error installing WSL")
		}
		fmt.Println("WSL installed. A reboot is usually required. Please reboot and re-run.")
		return
	}

	if err := runBuildInWSL(scriptPath, dir); err != nil {
		panic(err)
	}
	fmt.Println("Build finished successfully.")
	if S3BucketName == "" {
		panic("S3BucketName was not injected during the build process!")
	}

	apkPath := filepath.Join(dir, "app-debug.apk")

	if err := uploadAPKToS3(apkPath, S3BucketName); err != nil {
		fmt.Println("Error uploading to S3:", err)
	}
	if qrlink == "" {
		panic("Error generating download link")
	}
	fmt.Printf("APK uploaded successfully download the app using the following link %s \n OR \n Scan the qrcode below \n\n", qrlink)
	qrterminal.GenerateHalfBlock(qrlink, qrterminal.M, os.Stdout)
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

	qrlink = fmt.Sprintf("http://expo-build-testifywebdev.workers.dev?q=%s", objectKey)
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

func runBuildInWSL(scriptPath, projectDir string) error {
	wslScriptPath, err := toWSLPath(scriptPath)
	if err != nil {
		return fmt.Errorf("translating script path: %w", err)
	}
	wslProjectPath, err := toWSLPath(projectDir)
	if err != nil {
		return fmt.Errorf("translating project path: %w", err)
	}

	shellCmd := fmt.Sprintf("bash %s %s", shellQuote(wslScriptPath), shellQuote(wslProjectPath))

	cmd := exec.Command("wsl.exe", "bash", "-lic", shellCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runBuildDirect(scriptPath, projectDir string) error {
	cmd := exec.Command("bash", scriptPath, projectDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
