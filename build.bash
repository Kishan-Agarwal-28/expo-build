#!/bin/bash
set -e

# ==============================================================================
# 1. Load Java Environment
# ==============================================================================
if [[ -s "$HOME/.sdkman/bin/sdkman-init.sh" ]]; then
  source "$HOME/.sdkman/bin/sdkman-init.sh"
elif [[ -z "$JAVA_HOME" ]]; then
  if command -v java >/dev/null 2>&1; then
    export JAVA_HOME=$(dirname $(dirname $(readlink -f $(command -v java))))
  else
    echo "ERROR: Java is not installed or not in PATH inside WSL."
    exit 1
  fi
fi

# ==============================================================================
# 2. Android SDK Auto-Setup (WSL needs a Linux SDK, not the Windows one)
# ==============================================================================
export ANDROID_HOME="$HOME/Android/Sdk"
export PATH="$PATH:$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools"

if [[ ! -d "$ANDROID_HOME" ]]; then
  echo "Android SDK not found in WSL. Downloading and configuring (this will take a few minutes)..."
  
  # Ensure 'unzip' is installed (required to unpack the SDK tools)
  if ! command -v unzip >/dev/null 2>&1; then
    echo "ERROR: 'unzip' is required but not installed."
    echo "Please open a normal WSL terminal and run: sudo apt update && sudo apt install unzip -y"
    exit 1
  fi

  mkdir -p "$ANDROID_HOME/cmdline-tools"
  # Download official Android Command Line Tools for Linux
  wget -q "https://dl.google.com/android/repository/commandlinetools-linux-11076708_latest.zip" -O /tmp/cmdline-tools.zip
  unzip -q /tmp/cmdline-tools.zip -d "$ANDROID_HOME/cmdline-tools"
  rm /tmp/cmdline-tools.zip
  
  # Rename extracted folder to 'latest' (required by modern Android SDK setups)
  mv "$ANDROID_HOME/cmdline-tools/cmdline-tools" "$ANDROID_HOME/cmdline-tools/latest"
  
  echo "Accepting Android SDK licenses..."
  yes | "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" --licenses > /dev/null
  
  echo "Installing required platforms, build tools, and NDK (Please wait)..."
  "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" "platform-tools" "platforms;android-36" "build-tools;36.0.0" "ndk;27.1.12297006" > /dev/null
  echo "Android SDK setup complete!"
fi
# ==============================================================================

BUILD_DIR=~/build

echo "Copying files"

if [[ -n "$1" ]]; then
  initialPath="$1"
else
  echo "Could not find the path to copy files from. Please provide a path as an argument."
  exit 1
fi

detect_package_manager() {
  local path="$1"
  if [[ -f "$path/pnpm-lock.yaml" ]]; then
    echo "pnpm"
  elif [[ -f "$path/yarn.lock" ]]; then
    echo "yarn"
  elif [[ -f "$path/package-lock.json" ]]; then
    echo "npm"
  elif [[ -f "$path/bun.lock" ]]; then
    echo "bun"
  else
    echo "npm" 
  fi
}

pkgManager=$(detect_package_manager "$initialPath")

if [[ ! -f "$initialPath/pnpm-lock.yaml" && ! -f "$initialPath/yarn.lock" && ! -f "$initialPath/package-lock.json" && ! -f "$initialPath/bun.lock" ]]; then
  echo "Warning: no lockfile found in $initialPath. Defaulting to npm."
fi
echo "Detected package manager: $pkgManager"

case "$pkgManager" in
  pnpm) installCmd="pnpm install" ;;
  yarn) installCmd="yarn install" ;;
  npm)  installCmd="npm install" ;;
  bun)  installCmd="bun install" ;;
esac

mkdir -p "$BUILD_DIR"

# Using tar pipe instead of rsync to exclude unnecessary heavy directories
tar --exclude='node_modules' --exclude='android/.cxx' --exclude='android/build' -cf - -C "$initialPath" . | tar -xf - -C "$BUILD_DIR"

cd "$BUILD_DIR"

echo "Starting the build"
echo "Installing dependencies with $pkgManager"
if ! $installCmd; then
  echo "Dependency installation failed."
  exit 1
fi
echo "Dependencies installed successfully"

echo "Running expo prebuild"
if ! npx expo prebuild; then
  echo "Expo prebuild failed."
  exit 1
fi
echo "Prebuild completed successfully"
# ==============================================================================
# NEW: Inject Gradle limits to prevent Out-Of-Memory (OOM) crashes in WSL
# ==============================================================================
echo "Configuring Gradle memory limits..."
cd ./android
cat <<EOF >> gradle.properties
# Restrict Gradle JVM memory usage
org.gradle.jvmargs=-Xmx4g -XX:MaxMetaspaceSize=1g
# Restrict parallel workers to prevent CMake from eating all RAM
org.gradle.workers.max=2
# Enable caching and parallel builds (safely)
org.gradle.caching=true
org.gradle.parallel=true
EOF
# ==============================================================================

echo "Starting Gradle build"
if ! ./gradlew assembleDebug; then
  echo "Gradle build failed."
  exit 1
fi
echo "Gradle build completed successfully"


outputDir="$BUILD_DIR/android/app/build/outputs/apk/debug"

if [[ ! -d "$outputDir" ]]; then
  echo "Output directory not found: $outputDir"
  exit 1
fi

echo "Build complete. APK is ready at $outputDir"

# Copy the APK to the current directory
cp "$outputDir/app-debug.apk" "$initialPath/app-debug.apk"

echo "Build complete. APK is ready at $initialPath/app-debug.apk"

echo "Cleaning up..."
rm -rf "$BUILD_DIR"

exit 0