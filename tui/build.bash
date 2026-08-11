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
# 2. Setup Build Directory & Copy Files
# ==============================================================================
BUILD_DIR=~/build

if [[ -n "$1" ]]; then
  initialPath="$1"
else
  echo "Could not find the path to copy files from. Please provide a path as an argument."
  exit 1
fi

BUILD_TYPE="$2"

# $3 is the path of the actual Expo project relative to $initialPath.
# "." (or empty) means the project lives at the root of $initialPath, i.e.
# the classic single-repo case. Anything else (e.g. "apps/mobile") means
# $initialPath is a monorepo/turborepo root and the real app is nested.
APP_SUBDIR="${3:-.}"
if [[ -z "$APP_SUBDIR" ]]; then
  APP_SUBDIR="."
fi

# $4 is the build format: starts with "APK" or "AAB".
# Empty / unset means APK (backwards-compatible default).
BUILD_FORMAT="${4:-APK}"

if [[ "$BUILD_TYPE" == "Signing Report" ]]; then
  GRADLE_CMD="signingReport"
  ARTIFACT_NAME=""
  OUTPUT_SUBDIR=""
  OUTPUT_TYPE=""
elif [[ "$BUILD_FORMAT" == AAB* ]]; then
  # AAB (Android App Bundle) — for Play Store distribution
  if [[ "$BUILD_TYPE" == "Production" ]]; then
    GRADLE_CMD="bundleRelease"
    ARTIFACT_NAME="app-release.aab"
    OUTPUT_SUBDIR="release"
  else
    GRADLE_CMD="bundleDebug"
    ARTIFACT_NAME="app-debug.aab"
    OUTPUT_SUBDIR="debug"
  fi
  OUTPUT_TYPE="bundle"
else
  # APK — for direct sideloading
  if [[ "$BUILD_TYPE" == "Production" ]]; then
    GRADLE_CMD="assembleRelease"
    ARTIFACT_NAME="app-release.apk"
    OUTPUT_SUBDIR="release"
  else
    GRADLE_CMD="assembleDebug"
    ARTIFACT_NAME="app-debug.apk"
    OUTPUT_SUBDIR="debug"
  fi
  OUTPUT_TYPE="apk"
fi

detect_package_manager() {
  local path="$1"
  if [[ -f "$path/pnpm-lock.yaml" ]]; then echo "pnpm"
  elif [[ -f "$path/yarn.lock" ]]; then echo "yarn"
  elif [[ -f "$path/package-lock.json" ]]; then echo "npm"
  elif [[ -f "$path/bun.lock" ]]; then echo "bun"
  else echo "npm"; fi
}

# Package manager / lockfile is always resolved from the workspace root
# ($initialPath), since that's where a monorepo's lockfile lives.
pkgManager=$(detect_package_manager "$initialPath")
echo "Detected package manager: $pkgManager"

case "$pkgManager" in
  pnpm) installCmd="pnpm install" ;;
  yarn) installCmd="yarn install" ;;
  npm)  installCmd="npm install" ;;
  bun)  installCmd="bun install" ;;
esac

mkdir -p "$BUILD_DIR"
echo "Copying files..."
# Exclusions are intentionally basename-only (no leading path) so tar drops
# matching directories at ANY depth - e.g. apps/mobile/android/build and
# packages/foo/node_modules, not just top-level ones. That matters once
# there's more than one package under the root.
tar \
  --exclude='node_modules' \
  --exclude='.git' \
  --exclude='build' \
  --exclude='.cxx' \
  --exclude='Pods' \
  --exclude='.expo' \
  --exclude='.turbo' \
  --exclude='.next' \
  --exclude='dist' \
  -cf - -C "$initialPath" . | tar -xf - -C "$BUILD_DIR"
cd "$BUILD_DIR"

# APP_DIR is where expo prebuild / gradle actually run.
APP_DIR="$BUILD_DIR/$APP_SUBDIR"
if [[ ! -d "$APP_DIR" ]]; then
  echo "ERROR: Expected Expo project at '$APP_SUBDIR' but it wasn't found after copying."
  exit 1
fi

if [[ "$APP_SUBDIR" != "." ]]; then
  echo "Building monorepo project: $APP_SUBDIR"
fi

# ==============================================================================
# 3. Install Dependencies & Prebuild (Must happen BEFORE SDK Setup)
# ==============================================================================
echo "Installing dependencies with $pkgManager..."
if ! $installCmd; then
  echo "Dependency installation failed."
  exit 1
fi

echo "Running expo prebuild in $APP_SUBDIR..."
cd "$APP_DIR"
if ! npx expo prebuild; then
  echo "Expo prebuild failed."
  exit 1
fi
echo "Prebuild completed successfully"

# ==============================================================================
# 4. Auto-Detect Required Android SDK Versions
# ==============================================================================
echo "Auto-detecting required Android SDK versions from build.gradle..."

if [[ -f "$APP_DIR/android/build.gradle" ]]; then
  COMPILE_SDK=$(grep -oE 'compileSdk(Version)?\s*=?\s*[0-9]+' "$APP_DIR/android/build.gradle" | grep -oE '[0-9]+' | head -1)
  BUILD_TOOLS=$(grep -oE 'buildToolsVersion\s*=?\s*["'\''][0-9.]+["'\'']' "$APP_DIR/android/build.gradle" | grep -oE '[0-9.]+' | head -1)
  NDK_VERSION=$(grep -oE 'ndkVersion\s*=?\s*["'\''][0-9.]+["'\'']' "$APP_DIR/android/build.gradle" | grep -oE '[0-9.]+' | head -1)
fi

COMPILE_SDK=${COMPILE_SDK:-35}
BUILD_TOOLS=${BUILD_TOOLS:-35.0.0}
NDK_VERSION=${NDK_VERSION:-26.1.10909125}

echo "Detected Requirements:"
echo " -> Compile SDK:   $COMPILE_SDK"
echo " -> Build Tools:   $BUILD_TOOLS"
echo " -> NDK Version:   $NDK_VERSION"

# ==============================================================================
# 5. Provision Android SDK
# ==============================================================================
export ANDROID_HOME="$HOME/Android/Sdk"
export PATH="$PATH:$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools"

if [[ ! -d "$ANDROID_HOME/cmdline-tools/latest" ]]; then
  echo "Android SDK not found in WSL. Downloading..."
  if ! command -v unzip >/dev/null 2>&1; then
    echo "ERROR: 'unzip' is required. Run: sudo apt install unzip -y"
    exit 1
  fi

  mkdir -p "$ANDROID_HOME/cmdline-tools"
  wget -q "https://dl.google.com/android/repository/commandlinetools-linux-11076708_latest.zip" -O /tmp/cmdline-tools.zip
  unzip -q /tmp/cmdline-tools.zip -d "$ANDROID_HOME/cmdline-tools"
  rm /tmp/cmdline-tools.zip
  mv "$ANDROID_HOME/cmdline-tools/cmdline-tools" "$ANDROID_HOME/cmdline-tools/latest"
fi

echo "Accepting Android SDK licenses..."
yes | "$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" --licenses > /dev/null

echo "Installing platforms;android-$COMPILE_SDK, build-tools;$BUILD_TOOLS, and ndk;$NDK_VERSION..."
"$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" \
  "platform-tools" \
  "platforms;android-$COMPILE_SDK" \
  "build-tools;$BUILD_TOOLS" \
  "ndk;$NDK_VERSION" > /dev/null
echo "Android SDK setup complete!"

# ==============================================================================
# 6. Configure Gradle (Memory limits & SDK Location)
# ==============================================================================
echo "Configuring Gradle memory limits and local.properties..."
cd "$APP_DIR/android"

echo "" >> gradle.properties

cat <<EOF >> gradle.properties
# Restrict Gradle JVM memory usage
org.gradle.jvmargs=-Xmx4g -XX:MaxMetaspaceSize=1g
org.gradle.workers.max=2
org.gradle.caching=true
org.gradle.parallel=true

# ONLY build C++ for physical devices to save 50% memory and time!
reactNativeArchitectures=armeabi-v7a,arm64-v8a
EOF

echo "sdk.dir=$ANDROID_HOME" > local.properties

# ==============================================================================
# 7. Build the APK / Run target
# ==============================================================================
echo "Starting Gradle execution: $GRADLE_CMD..."

export NODE_OPTIONS="--max-old-space-size=8192"
export CMAKE_BUILD_PARALLEL_LEVEL=2
export NINJA_JOBS=2

if ! ./gradlew $GRADLE_CMD --info; then
  echo "Gradle execution failed."
  exit 1
fi

echo "Gradle $GRADLE_CMD completed successfully"

if [[ "$APP_SUBDIR" == "." ]]; then
  ORIGINAL_APP_PATH="$initialPath"
else
  ORIGINAL_APP_PATH="$initialPath/$APP_SUBDIR"
fi

if [[ -n "$ARTIFACT_NAME" ]]; then
  outputDir="$APP_DIR/android/app/build/outputs/$OUTPUT_TYPE/$OUTPUT_SUBDIR"

  if [[ ! -d "$outputDir" ]]; then
    echo "Output directory not found: $outputDir"
    exit 1
  fi

  mkdir -p "$ORIGINAL_APP_PATH"
  cp "$outputDir/$ARTIFACT_NAME" "$ORIGINAL_APP_PATH/$ARTIFACT_NAME"
  echo "Build complete. Artifact is ready at $ORIGINAL_APP_PATH/$ARTIFACT_NAME"
fi

echo "Cleaning up..."
cd "$initialPath"
rm -rf "$BUILD_DIR"

exit 0