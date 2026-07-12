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
if [[ "$BUILD_TYPE" == "Production" ]]; then
  GRADLE_CMD="assembleRelease"
  APK_NAME="app-release.apk"
  OUTPUT_SUBDIR="release"
elif [[ "$BUILD_TYPE" == "Signing Report" ]]; then
  GRADLE_CMD="signingReport"
  APK_NAME=""
else
  GRADLE_CMD="assembleDebug"
  APK_NAME="app-debug.apk"
  OUTPUT_SUBDIR="debug"
fi

detect_package_manager() {
  local path="$1"
  if [[ -f "$path/pnpm-lock.yaml" ]]; then echo "pnpm"
  elif [[ -f "$path/yarn.lock" ]]; then echo "yarn"
  elif [[ -f "$path/package-lock.json" ]]; then echo "npm"
  elif [[ -f "$path/bun.lock" ]]; then echo "bun"
  else echo "npm"; fi
}

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
tar --exclude='node_modules' --exclude='android/.cxx' --exclude='android/build' -cf - -C "$initialPath" . | tar -xf - -C "$BUILD_DIR"
cd "$BUILD_DIR"

# ==============================================================================
# 3. Install Dependencies & Prebuild (Must happen BEFORE SDK Setup)
# ==============================================================================
echo "Installing dependencies with $pkgManager..."
if ! $installCmd; then
  echo "Dependency installation failed."
  exit 1
fi

echo "Running expo prebuild..."
if ! npx expo prebuild; then
  echo "Expo prebuild failed."
  exit 1
fi
echo "Prebuild completed successfully"

# ==============================================================================
# 4. Auto-Detect Required Android SDK Versions
# ==============================================================================
echo "Auto-detecting required Android SDK versions from build.gradle..."

if [[ -f "android/build.gradle" ]]; then
  COMPILE_SDK=$(grep -oE 'compileSdk(Version)?\s*=?\s*[0-9]+' android/build.gradle | grep -oE '[0-9]+' | head -1)
  BUILD_TOOLS=$(grep -oE 'buildToolsVersion\s*=?\s*["'\''][0-9.]+["'\'']' android/build.gradle | grep -oE '[0-9.]+' | head -1)
  NDK_VERSION=$(grep -oE 'ndkVersion\s*=?\s*["'\''][0-9.]+["'\'']' android/build.gradle | grep -oE '[0-9.]+' | head -1)
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
cd ./android

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

if [[ -n "$APK_NAME" ]]; then
  outputDir="$BUILD_DIR/android/app/build/outputs/apk/$OUTPUT_SUBDIR"

  if [[ ! -d "$outputDir" ]]; then
    echo "Output directory not found: $outputDir"
    exit 1
  fi

  cp "$outputDir/$APK_NAME" "$initialPath/$APK_NAME"
  echo "Build complete. APK is ready at $initialPath/$APK_NAME"
fi

echo "Cleaning up..."
cd "$initialPath"
rm -rf "$BUILD_DIR"

exit 0