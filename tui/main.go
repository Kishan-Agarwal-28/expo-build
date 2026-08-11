package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	gobanner "github.com/Kishan-Agarwal-28/go-banner"
	zone "github.com/lrstanley/bubblezone/v2"
	qrterminal "github.com/mdp/qrterminal/v3"

	"github.com/kishan-agarwal-28/expo-tui/db"
	"github.com/kishan-agarwal-28/expo-tui/runner"
	"github.com/kishan-agarwal-28/expo-tui/tracker"
)

// ─── Palette ──────────────────────────────────────────────────────────────────

var (
	colorPrimary = lipgloss.Color("#7C3AED")
	colorAccent  = lipgloss.Color("#A78BFA")
	colorSuccess = lipgloss.Color("#34D399")
	colorError   = lipgloss.Color("#F87171")
	colorWarning = lipgloss.Color("#FBBF24")
	colorMuted   = lipgloss.Color("#6B7280")
	colorBorder  = lipgloss.Color("#374151")
	colorText    = lipgloss.Color("#F9FAFB")
	colorSubtext = lipgloss.Color("#9CA3AF")
	colorHighBg  = lipgloss.Color("#312E81")
	colorHighFg  = lipgloss.Color("#EDE9FE")
)

// ─── Tab ──────────────────────────────────────────────────────────────────────

type Tab int

const (
	buildTab Tab = iota
	historyTab
	shareTab
	settingsTab
)

func (t Tab) String() string {
	switch t {
	case buildTab:
		return "Build"
	case historyTab:
		return "History"
	case shareTab:
		return "Share"
	case settingsTab:
		return "Settings"
	}
	return "?"
}

// ─── Focus ────────────────────────────────────────────────────────────────────

type Focus int

const (
	FocusSidebar Focus = iota
	FocusContent
)

// ─── Build states ─────────────────────────────────────────────────────────────

type BuildState int

const (
	StateIdle      BuildState = iota
	StatePromptApp            // monorepo: pick which Expo app
	StatePromptType
	StatePromptFormat
	StatePromptDelivery
	StateBuilding
	StateDone
)

// ─── Settings fields ──────────────────────────────────────────────────────────

type SettingsField int

const (
	sfS3Bucket SettingsField = iota
	sfAWSEndpoint
	sfAWSKey
	sfAWSSecret
	sfAWSRegion
	sfBaseURL
	sfDelivery
	sfGradleHeap
	sfGradleWorkers
	sfGradleParallel
	sfGradleCaching
	sfRNArchs
	sfNodeMem
	sfGitTracking
	sfSave
	sfCount
)

// sensitiveField returns true for fields that should be masked by default.
func sensitiveField(sf SettingsField) bool {
	return sf == sfAWSKey || sf == sfAWSSecret
}

func (sf SettingsField) Label() string {
	labels := [sfCount]string{
		"S3 Bucket Name",
		"AWS Endpoint",
		"AWS Access Key",
		"AWS Secret Key",
		"AWS Region",
		"Base URL (download link)",
		"Delivery Method (local/s3)",
		"Gradle JVM Max Heap",
		"Gradle Max Workers",
		"Gradle Parallel (true/false)",
		"Gradle Caching  (true/false)",
		"RN Architectures",
		"Node Max Old Space (MB)",
		"Git Tracking    (true/false)",
		"[ Save Settings ]",
	}
	return labels[sf]
}

// ─── Share states ─────────────────────────────────────────────────────────────

type ShareState int

const (
	SharePickBuild ShareState = iota
	SharePickMethod
	ShareServing // local HTTP server is running
	ShareUploading
	ShareDone
)

// ─── Messages ─────────────────────────────────────────────────────────────────

type tickMsg struct{}
type settingsSavedMsg struct{}
type historyLoadedMsg struct{ builds []db.Build }
type appIDsLoadedMsg struct{ ids []string }
type shareBuildsLoadedMsg struct{ builds []db.Build }
type trackerDoneMsg struct{ err error }
type restoreDoneMsg struct {
	outDir string
	err    error
}
type appsFoundMsg struct{ apps []string }
type localServerReadyMsg struct {
	url    string
	stopFn func()
}
type localServerDoneMsg struct{ err error }

// ─── Model ────────────────────────────────────────────────────────────────────

type model struct {
	// layout
	width        int
	height       int
	ready        bool
	headerHeight int

	// navigation
	tabActive Tab
	focus     Focus

	// data
	database *db.DB
	appID    string
	settings db.Settings

	// ── build tab ──
	buildState     BuildState
	buildLogs      *strings.Builder // pointer — never copy by value
	buildViewport  viewport.Model
	sharedLog      *runner.SharedLog
	currentBuildID string
	promptOptions  []string
	promptCursor   int
	promptLabel    string
	selectedType   string
	selectedFmt    string
	buildSpinner   int
	// monorepo app selection
	foundApps      []string // relative paths to detected Expo projects
	selectedAppDir string   // chosen app rel-dir

	// ── history tab ──
	// level 0: app IDs list | level 1: builds for app | level 2: build detail
	historyLevel         int
	historyAppIDs        []string
	historyAppIDCursor   int
	selectedHistoryApp   string
	builds               []db.Build // builds for selectedHistoryApp
	historyCursor        int
	historyDetail        *db.Build
	historyRestoreStatus string // feedback message after restore

	// ── share tab ──
	shareState    ShareState
	shareBuilds   []db.Build
	shareCursor   int
	shareSelected *db.Build
	shareURL      string
	shareStopFn   func() // stops the local HTTP server
	shareMessage  string // status / error text

	// ── settings tab ──
	settingsFields  [sfCount]string
	settingsCursor  SettingsField
	settingsEditing bool
	settingsSaved   bool
	settingsUnmask  [sfCount]bool // true = show plain text for that field
}

// ─── Init ─────────────────────────────────────────────────────────────────────

func initialModel(database *db.DB, appID string) model {
	settings, _ := database.GetSettings(appID)
	m := model{
		database:       database,
		appID:          appID,
		settings:       settings,
		tabActive:      buildTab,
		focus:          FocusSidebar,
		buildState:     StateIdle,
		buildLogs:      new(strings.Builder),
		shareState:     SharePickBuild,
		selectedAppDir: ".",
	}
	m.loadSettingsFields()
	return m
}

func (m *model) loadSettingsFields() {
	m.settingsFields[sfS3Bucket] = m.settings.S3BucketName
	m.settingsFields[sfAWSEndpoint] = m.settings.AWSEndpoint
	m.settingsFields[sfAWSKey] = m.settings.AWSAccessKey
	m.settingsFields[sfAWSSecret] = m.settings.AWSSecretKey
	m.settingsFields[sfAWSRegion] = m.settings.AWSRegion
	m.settingsFields[sfBaseURL] = m.settings.BaseURL
	m.settingsFields[sfDelivery] = m.settings.DeliveryMethod
	m.settingsFields[sfGradleHeap] = m.settings.GradleMaxHeap
	m.settingsFields[sfGradleWorkers] = m.settings.GradleWorkers
	m.settingsFields[sfGradleParallel] = boolStr(m.settings.GradleParallel)
	m.settingsFields[sfGradleCaching] = boolStr(m.settings.GradleCaching)
	m.settingsFields[sfRNArchs] = m.settings.RNArchitectures
	m.settingsFields[sfNodeMem] = m.settings.NodeMaxOldSpace
	m.settingsFields[sfGitTracking] = boolStr(m.settings.GitTracking)
	m.settingsFields[sfSave] = ""
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		loadAppIDs(m.database),
		loadShareBuilds(m.database),
		tea.Every(100*time.Millisecond, func(_ time.Time) tea.Msg { return tickMsg{} }),
	)
}

// ─── Update ───────────────────────────────────────────────────────────────────

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		_, m.headerHeight = m.buildHeader()
		if !m.ready {
			m.buildViewport = viewport.New(viewport.WithWidth(10), viewport.WithHeight(10))
			m.buildViewport.LeftGutterFunc = func(info viewport.GutterContext) string {
				if info.Soft {
					return "     | "
				}
				if info.Index >= info.TotalLines {
					return "   ~ | "
				}
				return fmt.Sprintf("%5d | ", info.Index+1)
			}
			m.buildViewport.HighlightStyle = lipgloss.NewStyle().
				Foreground(colorHighFg).Background(colorHighBg)
			m.ready = true
		}
		m.syncViewportSize()

	case tickMsg:
		m.buildSpinner++
		if m.settingsSaved {
			m.settingsSaved = false
		}
		if m.sharedLog != nil {
			snap := m.sharedLog.String()
			if snap != m.buildLogs.String() {
				m.buildLogs = new(strings.Builder)
				m.buildLogs.WriteString(snap)
				m.buildViewport.SetContent(snap)
				m.buildViewport.GotoBottom()
			}
		}
		cmds = append(cmds,
			tea.Every(100*time.Millisecond, func(_ time.Time) tea.Msg { return tickMsg{} }),
		)

	case appIDsLoadedMsg:
		m.historyAppIDs = msg.ids

	case historyLoadedMsg:
		m.builds = msg.builds

	case shareBuildsLoadedMsg:
		m.shareBuilds = msg.builds

	case appsFoundMsg:
		m.foundApps = msg.apps
		if len(m.foundApps) <= 1 {
			// Single (or no) app: skip the picker, use the found path or "."
			if len(m.foundApps) == 1 {
				m.selectedAppDir = m.foundApps[0]
			}
			m.buildState = StatePromptType
			m.promptLabel = "Select build type"
			m.promptOptions = []string{"Debug", "Production", "Signing Report"}
			m.promptCursor = 0
		} else {
			m.buildState = StatePromptApp
			m.promptLabel = "Multiple Expo apps found -- pick one"
			m.promptOptions = m.foundApps
			m.promptCursor = 0
		}

	case runner.BuildDoneMsg:
		m.sharedLog = nil
		if msg.Err != "" {
			m.buildLogs.WriteString("\n[FAILED] ");m.buildLogs.WriteString(msg.Err);m.buildLogs.WriteString("\n")
			if m.database != nil && m.currentBuildID != "" {
				_ = m.database.SetBuildStatus(m.currentBuildID, "failed", "")
			}
		} else {
			status := "failed"
			if msg.Success {
				status = "success"
			}
			if m.database != nil && m.currentBuildID != "" {
				_ = m.database.SetBuildStatus(m.currentBuildID, status, msg.ArtifactPath)
			}
			if msg.Success {
				m.buildLogs.WriteString("\n[OK] Build succeeded!\n")
				cmds = append(cmds, loadAppIDs(m.database), loadShareBuilds(m.database))
				if m.settings.GitTracking && msg.ArtifactPath != "" {
					buildID := m.currentBuildID
					artPath := msg.ArtifactPath
					appID := m.appID
					cmds = append(cmds, func() tea.Msg {
						err := tracker.CommitArtifact(appID, buildID, artPath, func(_ string) {})
						return trackerDoneMsg{err: err}
					})
				}
			} else {
				m.buildLogs.WriteString("\n[FAILED] Build process exited with errors.\n")
			}
		}
		m.buildState = StateDone
		m.buildViewport.SetContent(m.buildLogs.String())
		m.buildViewport.GotoBottom()
		// reload history
		if m.selectedHistoryApp != "" {
			cmds = append(cmds, loadBuildsForApp(m.database, m.selectedHistoryApp))
		}

	case trackerDoneMsg:
		if msg.err != nil {
			m.buildLogs.WriteString("[WARN] Git tracker: ");m.buildLogs.WriteString(msg.err.Error());m.buildLogs.WriteString("\n")
		} else {
			m.buildLogs.WriteString("[GIT] APK committed to tracking branch.\n")
		}
		m.buildViewport.SetContent(m.buildLogs.String())
		m.buildViewport.GotoBottom()

	case restoreDoneMsg:
		if msg.err != nil {
			m.historyRestoreStatus = "[ERR] Restore failed: " + msg.err.Error()
		} else {
			m.historyRestoreStatus = "[OK] Restored to: " + msg.outDir
		}

	case settingsSavedMsg:
		m.settings, _ = m.database.GetSettings(m.appID)
		m.loadSettingsFields()
		m.settingsSaved = true

	case localServerReadyMsg:
		m.shareURL = msg.url
		m.shareStopFn = msg.stopFn
		m.shareState = ShareServing
		m.shareMessage = "Waiting for download... (q to cancel)"

	case localServerDoneMsg:
		m.shareState = ShareDone
		if m.shareStopFn != nil {
			m.shareStopFn()
			m.shareStopFn = nil
		}
		if msg.err != nil {
			m.shareMessage = "[ERR] " + msg.err.Error()
		} else {
			m.shareMessage = "[OK] Transfer complete!"
		}

	case tea.KeyPressMsg:
		return m.handleKey(msg, cmds)

	case tea.MouseClickMsg:
		m.handleMouse(msg)
	}

	// Forward to viewport when build logs visible
	if m.ready &&
		m.tabActive == buildTab &&
		m.focus == FocusContent &&
		m.buildState != StatePromptApp &&
		m.buildState != StatePromptType &&
		m.buildState != StatePromptFormat &&
		m.buildState != StatePromptDelivery {
		var vpCmd tea.Cmd
		m.buildViewport, vpCmd = m.buildViewport.Update(msg)
		cmds = append(cmds, vpCmd)
	}

	return m, tea.Batch(cmds...)
}

// ─── Key handling ─────────────────────────────────────────────────────────────

func (m model) handleKey(msg tea.KeyPressMsg, cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	key := msg.String()

	if key == "ctrl+c" {
		if m.shareStopFn != nil {
			m.shareStopFn()
		}
		return m, tea.Quit
	}

	// Settings edit mode
	if m.tabActive == settingsTab && m.settingsEditing {
		return m.handleSettingsEdit(key, cmds)
	}

	// Build prompts
	if m.tabActive == buildTab {
		switch m.buildState {
		case StateIdle, StateDone:
			if key == "n" || (key == "enter" && m.focus == FocusContent) {
				return m.startFindApps(cmds)
			}
		case StatePromptApp, StatePromptType, StatePromptFormat, StatePromptDelivery:
			return m.handlePromptKey(key, cmds)
		}
	}

	// Share tab: stop server with q
	if m.tabActive == shareTab && m.shareState == ShareServing && key == "q" {
		if m.shareStopFn != nil {
			m.shareStopFn()
			m.shareStopFn = nil
		}
		m.shareState = ShareDone
		m.shareMessage = "Server stopped."
		return m, tea.Batch(cmds...)
	}

	// Global
	switch key {
	case "q", "esc":
		if m.tabActive == historyTab && m.historyLevel > 0 {
			m.historyLevel--
			m.historyRestoreStatus = ""
			if m.historyLevel == 0 {
				m.historyDetail = nil
			}
			return m, tea.Batch(cmds...)
		}
		if m.tabActive != buildTab ||
			m.buildState == StateIdle || m.buildState == StateDone {
			if m.shareStopFn != nil {
				m.shareStopFn()
			}
			return m, tea.Quit
		}

	case "tab", "right", "l":
		m.focus = FocusContent
	case "shift+tab", "left", "h":
		m.focus = FocusSidebar

	case "1":
		m.tabActive = buildTab
	case "2":
		m.tabActive = historyTab
	case "3":
		m.tabActive = shareTab
	case "4":
		m.tabActive = settingsTab

	case "up", "k":
		if m.focus == FocusSidebar {
			if m.tabActive > 0 {
				m.tabActive--
			}
		} else {
			switch m.tabActive {
			case historyTab:
				switch m.historyLevel {
				case 0:
					if m.historyAppIDCursor > 0 {
						m.historyAppIDCursor--
					}
				case 1:
					if m.historyCursor > 0 {
						m.historyCursor--
					}
				}
			case shareTab:
				if m.shareState == SharePickBuild && m.shareCursor > 0 {
					m.shareCursor--
				}
			case settingsTab:
				if int(m.settingsCursor) > 0 {
					m.settingsCursor--
				}
			}
		}

	case "down", "j":
		if m.focus == FocusSidebar {
			if m.tabActive < settingsTab {
				m.tabActive++
			}
		} else {
			switch m.tabActive {
			case historyTab:
				switch m.historyLevel {
				case 0:
					if m.historyAppIDCursor < len(m.historyAppIDs)-1 {
						m.historyAppIDCursor++
					}
				case 1:
					if m.historyCursor < len(m.builds)-1 {
						m.historyCursor++
					}
				}
			case shareTab:
				if m.shareState == SharePickBuild &&
					m.shareCursor < len(m.shareBuilds)-1 {
					m.shareCursor++
				}
			case settingsTab:
				if int(m.settingsCursor) < int(sfCount)-1 {
					m.settingsCursor++
				}
			}
		}

	case "enter":
		switch m.tabActive {
		case historyTab:
			if m.focus == FocusContent {
				return m.handleHistoryEnter(cmds)
			}
		case shareTab:
			if m.focus == FocusContent {
				return m.handleShareEnter(cmds)
			}
		case settingsTab:
			if m.focus == FocusContent {
				if m.settingsCursor == sfSave {
					return m.saveSettings(cmds)
				}
				m.settingsEditing = true
			}
		}

	case "r":
		// Restore shortcut in history level 2
		if m.tabActive == historyTab && m.historyLevel == 2 && m.historyDetail != nil {
			return m.doRestore(cmds)
		}
		// Reload share builds
		if m.tabActive == shareTab {
			cmds = append(cmds, loadShareBuilds(m.database))
		}

	case "m":
		// Toggle mask for sensitive settings fields
		if m.tabActive == settingsTab && m.focus == FocusContent {
			if sensitiveField(m.settingsCursor) {
				m.settingsUnmask[m.settingsCursor] = !m.settingsUnmask[m.settingsCursor]
			}
		}
	}

	return m, tea.Batch(cmds...)
}

func (m model) handleHistoryEnter(cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	switch m.historyLevel {
	case 0:
		if len(m.historyAppIDs) == 0 {
			return m, tea.Batch(cmds...)
		}
		m.selectedHistoryApp = m.historyAppIDs[m.historyAppIDCursor]
		m.historyLevel = 1
		m.historyCursor = 0
		m.historyDetail = nil
		cmds = append(cmds, loadBuildsForApp(m.database, m.selectedHistoryApp))
	case 1:
		if len(m.builds) == 0 {
			return m, tea.Batch(cmds...)
		}
		b := m.builds[m.historyCursor]
		m.historyDetail = &b
		m.historyLevel = 2
		m.historyRestoreStatus = ""
	case 2:
		return m.doRestore(cmds)
	}
	return m, tea.Batch(cmds...)
}

func (m model) doRestore(cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	if m.historyDetail == nil {
		return m, tea.Batch(cmds...)
	}
	b := m.historyDetail
	dest := projectDir()
	cmds = append(cmds, func() tea.Msg {
		outDir, err := tracker.RestoreArtifact(b.AppID, b.ID, dest)
		return restoreDoneMsg{outDir: outDir, err: err}
	})
	m.historyRestoreStatus = "Restoring..."
	return m, tea.Batch(cmds...)
}

func (m model) handleShareEnter(cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	switch m.shareState {
	case SharePickBuild:
		if len(m.shareBuilds) == 0 {
			return m, tea.Batch(cmds...)
		}
		m.shareSelected = &m.shareBuilds[m.shareCursor]
		m.shareState = SharePickMethod
		m.promptLabel = "How to share this APK?"
		m.promptOptions = []string{"Local WiFi (instant, no internet)", "Upload to S3"}
		m.promptCursor = 0
	case SharePickMethod:
		choice := m.promptOptions[m.promptCursor]
		if strings.HasPrefix(choice, "Local") {
			return m.startLocalServer(cmds)
		}
		return m.startS3Upload(cmds)
	}
	return m, tea.Batch(cmds...)
}

func (m model) startLocalServer(cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	if m.shareSelected == nil {
		return m, tea.Batch(cmds...)
	}
	apkPath := m.shareSelected.ArtifactPath
	m.shareMessage = "Starting local server..."
	cmds = append(cmds, func() tea.Msg {
		url, stopFn, err := serveAPKLocal(apkPath)
		if err != nil {
			return localServerDoneMsg{err: err}
		}
		return localServerReadyMsg{url: url, stopFn: stopFn}
	})
	return m, tea.Batch(cmds...)
}

func (m model) startS3Upload(cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	if m.shareSelected == nil || m.settings.S3BucketName == "" {
		m.shareState = ShareDone
		m.shareMessage = "[ERR] S3 not configured. Set credentials in Settings tab."
		return m, tea.Batch(cmds...)
	}
	m.shareState = ShareUploading
	m.shareMessage = "Uploading to S3..."
	apkPath := m.shareSelected.ArtifactPath
	s := m.settings
	cmds = append(cmds, func() tea.Msg {
		url, err := uploadToS3(apkPath, s)
		if err != nil {
			return localServerDoneMsg{err: err}
		}
		return localServerReadyMsg{url: url, stopFn: func() {}}
	})
	return m, tea.Batch(cmds...)
}

func (m model) handleSettingsEdit(key string, cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	idx := m.settingsCursor
	switch key {
	case "enter", "esc":
		m.settingsEditing = false
	case "backspace":
		v := m.settingsFields[idx]
		if len(v) > 0 {
			runes := []rune(v)
			m.settingsFields[idx] = string(runes[:len(runes)-1])
		}
	default:
		if len([]rune(key)) == 1 {
			m.settingsFields[idx] += key
		}
	}
	return m, tea.Batch(cmds...)
}

func (m model) saveSettings(cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	s := m.settings
	s.S3BucketName = m.settingsFields[sfS3Bucket]
	s.AWSEndpoint = m.settingsFields[sfAWSEndpoint]
	s.AWSAccessKey = m.settingsFields[sfAWSKey]
	s.AWSSecretKey = m.settingsFields[sfAWSSecret]
	s.AWSRegion = m.settingsFields[sfAWSRegion]
	s.BaseURL = m.settingsFields[sfBaseURL]
	s.DeliveryMethod = m.settingsFields[sfDelivery]
	s.GradleMaxHeap = m.settingsFields[sfGradleHeap]
	s.GradleWorkers = m.settingsFields[sfGradleWorkers]
	s.GradleParallel = parseBool(m.settingsFields[sfGradleParallel])
	s.GradleCaching = parseBool(m.settingsFields[sfGradleCaching])
	s.RNArchitectures = m.settingsFields[sfRNArchs]
	s.NodeMaxOldSpace = m.settingsFields[sfNodeMem]
	s.GitTracking = parseBool(m.settingsFields[sfGitTracking])
	_ = m.database.SaveSettings(s)
	m.settings = s
	cmds = append(cmds, func() tea.Msg { return settingsSavedMsg{} })
	return m, tea.Batch(cmds...)
}

func (m model) startFindApps(cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	m.focus = FocusContent
	cmds = append(cmds, func() tea.Msg {
		apps := findExpoApps(projectDir())
		return appsFoundMsg{apps: apps}
	})
	return m, tea.Batch(cmds...)
}

func (m model) handlePromptKey(key string, cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.promptCursor > 0 {
			m.promptCursor--
		}
	case "down", "j":
		if m.promptCursor < len(m.promptOptions)-1 {
			m.promptCursor++
		}
	case "enter":
		choice := m.promptOptions[m.promptCursor]
		switch m.buildState {
		case StatePromptApp:
			m.selectedAppDir = choice
			m.buildState = StatePromptType
			m.promptLabel = "Select build type"
			m.promptOptions = []string{"Debug", "Production", "Signing Report"}
			m.promptCursor = 0
		case StatePromptType:
			m.selectedType = choice
			if choice == "Signing Report" {
				return m.launchBuild("", cmds)
			}
			m.buildState = StatePromptFormat
			m.promptLabel = "Select output format"
			m.promptOptions = []string{
				"APK -- sideload directly onto a device",
				"AAB -- upload to Play Store",
			}
			m.promptCursor = 0
		case StatePromptFormat:
			m.selectedFmt = choice
			m.buildState = StatePromptDelivery
			m.promptLabel = "Delivery method after build"
			m.promptOptions = []string{
				"Local WiFi",
				"Upload to S3",
				"Skip",
			}
			m.promptCursor = 0
		case StatePromptDelivery:
			return m.launchBuild(choice, cmds)
		}
	case "esc":
		m.buildState = StateIdle
	}
	return m, tea.Batch(cmds...)
}

func (m model) launchBuild(_ string, cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	m.buildState = StateBuilding
	m.buildLogs = new(strings.Builder)
	header := fmt.Sprintf(">> Build  Type: %-20s Format: %s  App: %s\n\n",
		m.selectedType, m.selectedFmt, m.selectedAppDir)
	m.buildLogs.WriteString(header)
	m.buildViewport.SetContent(m.buildLogs.String())
	m.buildViewport.GotoBottom()

	buildID, _ := m.database.InsertBuild(m.appID, m.selectedType, m.selectedFmt)
	m.currentBuildID = buildID

	opts := runner.BuildOptions{
		ProjectDir: projectDir(),
		AppRelDir:  m.selectedAppDir,
		BuildType:  m.selectedType,
		Format:     m.selectedFmt,
		ScriptBody: buildScript,
	}
	sharedLog, doneCmd := runner.Start(opts)
	m.sharedLog = sharedLog
	cmds = append(cmds, doneCmd)
	return m, tea.Batch(cmds...)
}

func (m *model) handleMouse(msg tea.MouseClickMsg) {
	if zone.Get("sidebar").InBounds(msg) {
		m.focus = FocusSidebar
		if zone.Get("tab_build").InBounds(msg) {
			m.tabActive = buildTab
		} else if zone.Get("tab_history").InBounds(msg) {
			m.tabActive = historyTab
		} else if zone.Get("tab_share").InBounds(msg) {
			m.tabActive = shareTab
		} else if zone.Get("tab_settings").InBounds(msg) {
			m.tabActive = settingsTab
		}
	} else if zone.Get("content").InBounds(msg) {
		m.focus = FocusContent
	}
}

// ─── View ─────────────────────────────────────────────────────────────────────

func (m model) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	if m.width == 0 || !m.ready {
		v.SetContent(lipgloss.NewStyle().Foreground(colorAccent).
			Render("  Initializing expo-build TUI..."))
		return v
	}

	inner := max(0, m.width-6)
	header, _ := m.buildHeader()
	sidebarWidth := 22
	contentWidth := max(0, inner-sidebarWidth-1)
	innerH := max(0, m.height-m.headerHeight-4)

	// sidebar tabs
	activeBg := colorPrimary
	if m.focus == FocusContent {
		activeBg = lipgloss.Color("#4B5563")
	}
	activeSt := lipgloss.NewStyle().
		Foreground(colorText).Background(activeBg).
		Padding(0, 1).MarginBottom(1).Width(sidebarWidth - 2).Bold(true)
	inactiveSt := lipgloss.NewStyle().
		Foreground(colorSubtext).
		Padding(0, 1).MarginBottom(1).Width(sidebarWidth - 2)

	icons := map[Tab]string{
		buildTab: "[B]", historyTab: "[H]", shareTab: "[S]", settingsTab: "[*]",
	}
	var renderedTabs []string
	for _, t := range []Tab{buildTab, historyTab, shareTab, settingsTab} {
		lbl := icons[t] + " " + t.String()
		if t == m.tabActive {
			renderedTabs = append(renderedTabs, zone.Mark("tab_"+strings.ToLower(t.String()), activeSt.Render(lbl)))
		} else {
			renderedTabs = append(renderedTabs, zone.Mark("tab_"+strings.ToLower(t.String()), inactiveSt.Render(lbl)))
		}
	}
	tabMenu := lipgloss.JoinVertical(lipgloss.Left, renderedTabs...)

	spinnerStr := ""
	if m.buildState == StateBuilding {
		frames := []string{"|", "/", "-", "\\"}
		spinnerStr = lipgloss.NewStyle().Foreground(colorWarning).
			Render(frames[m.buildSpinner%len(frames)] + " Building...")
	}

	sidebarBody := lipgloss.JoinVertical(lipgloss.Left, tabMenu, "", spinnerStr)
	sidebar := lipgloss.NewStyle().
		Width(sidebarWidth).Height(innerH).
		Border(lipgloss.NormalBorder(), false, true, false, false).
		BorderForeground(colorBorder).
		Render(sidebarBody)
	sidebar = zone.Mark("sidebar", sidebar)

	var content string
	switch m.tabActive {
	case buildTab:
		content = m.viewBuildTab(contentWidth, innerH)
	case historyTab:
		content = m.viewHistoryTab(contentWidth, innerH)
	case shareTab:
		content = m.viewShareTab(contentWidth, innerH)
	case settingsTab:
		content = m.viewSettingsTab(contentWidth)
	}

	contentBox := lipgloss.NewStyle().
		Width(contentWidth).Height(innerH).
		PaddingLeft(1).PaddingTop(1).Render(content)
	contentBox = zone.Mark("content", contentBox)

	body := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, contentBox)
	layout := lipgloss.JoinVertical(lipgloss.Left, header, body)

	outer := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder(), true, true).
		BorderForeground(colorPrimary).
		Height(m.height).Width(m.width).Padding(1, 2)

	v.SetContent(zone.Scan(outer.Render(layout)))
	return v
}

// ─── Build tab ────────────────────────────────────────────────────────────────

func (m model) viewBuildTab(w, h int) string {
	switch m.buildState {
	case StateIdle:
		return m.viewBuildIdle(w)
	case StatePromptApp, StatePromptType, StatePromptFormat, StatePromptDelivery:
		return m.viewPrompt(w)
	case StateBuilding, StateDone:
		return m.viewBuildLogs()
	}
	return ""
}

func (m model) viewBuildIdle(w int) string {
	sub := lipgloss.NewStyle().Foreground(colorSubtext).Render
	bold := lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render
	acc := lipgloss.NewStyle().Foreground(colorAccent).Bold(true).Render

	lines := []string{
		acc("No active build"),
		"",
		sub("Press ") + bold("n") + sub(" or ") + bold("Enter") + sub(" to start a new build"),
		sub("Tabs: ") + bold("1") + sub("=Build ") + bold("2") + sub("=History ") +
			bold("3") + sub("=Share ") + bold("4") + sub("=Settings"),
		"",
		sub("Navigate: ") + bold("up/down/j/k") + sub(" | Focus: ") + bold("l/->/tab"),
	}
	return lipgloss.NewStyle().
		Width(w-4).Padding(2, 3).
		Border(lipgloss.RoundedBorder(), true).BorderForeground(colorBorder).
		Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

func (m model) viewPrompt(w int) string {
	titleSt := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	selSt := lipgloss.NewStyle().Foreground(colorText).Background(colorHighBg).
		PaddingLeft(1).Bold(true)
	normSt := lipgloss.NewStyle().Foreground(colorSubtext).PaddingLeft(2)

	var rows []string
	rows = append(rows, titleSt.Render("  >> "+m.promptLabel), "")
	for i, opt := range m.promptOptions {
		if i == m.promptCursor {
			rows = append(rows, selSt.Render("> "+opt))
		} else {
			rows = append(rows, normSt.Render("  "+opt))
		}
	}
	rows = append(rows, "",
		lipgloss.NewStyle().Foreground(colorMuted).
			Render("  [up/down] navigate   [Enter] select   [Esc] cancel"))

	return lipgloss.NewStyle().
		Width(w-4).Padding(1, 2).
		Border(lipgloss.RoundedBorder(), true).BorderForeground(colorPrimary).
		Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

func (m model) viewBuildLogs() string {
	m.syncViewportSize()
	return lipgloss.JoinVertical(lipgloss.Left,
		m.vpHeader(),
		m.buildViewport.View(),
		m.vpFooter(),
	)
}

// ─── History tab ──────────────────────────────────────────────────────────────

func (m model) viewHistoryTab(w, h int) string {
	titleSt := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	subSt := lipgloss.NewStyle().Foreground(colorSubtext).PaddingLeft(1)
	selSt := lipgloss.NewStyle().Foreground(colorText).Background(colorHighBg).
		PaddingLeft(1).Bold(true)
	muteSt := lipgloss.NewStyle().Foreground(colorMuted)
	sep := strings.Repeat("-", w-6)

	switch m.historyLevel {
	// ── level 0: app ID list ──
	case 0:
		rows := []string{titleSt.Render("Apps (build history)"), sep, ""}
		if len(m.historyAppIDs) == 0 {
			rows = append(rows, subSt.Render("No builds yet. Start a build first."))
		}
		for i, id := range m.historyAppIDs {
			line := fmt.Sprintf("  %s", id)
			if i == m.historyAppIDCursor && m.focus == FocusContent {
				rows = append(rows, selSt.Render(line))
			} else {
				rows = append(rows, subSt.Render(line))
			}
		}
		rows = append(rows, "", muteSt.Render("  [up/down] navigate   [Enter] expand   [Esc/q] back"))
		return lipgloss.JoinVertical(lipgloss.Left, rows...)

	// ── level 1: builds for selected app ──
	case 1:
		rows := []string{
			titleSt.Render("Builds for: " + m.selectedHistoryApp),
			sep,
			subSt.Render(fmt.Sprintf("  %-4s  %-10s %-6s %-8s  %s",
				"St", "Type", "Fmt", "Status", "Date")),
			sep,
		}
		if len(m.builds) == 0 {
			rows = append(rows, subSt.Render("  No builds recorded."))
		}
		for i, b := range m.builds {
			st := buildStatusGlyph(b.Status)
			ts := b.CreatedAt.Format("01-02 15:04")
			fmtShort := b.Format
			if len(fmtShort) > 3 {
				fmtShort = fmtShort[:3]
			}
			line := fmt.Sprintf("  %-4s  %-10s %-6s %-8s  %s",
				st, b.BuildType, fmtShort, b.Status, ts)
			if i == m.historyCursor && m.focus == FocusContent {
				rows = append(rows, selSt.Render(line))
			} else {
				rows = append(rows, subSt.Render(line))
			}
		}
		rows = append(rows, "", muteSt.Render("  [Enter] detail   [Esc/q] back"))
		return lipgloss.JoinVertical(lipgloss.Left, rows...)

	// ── level 2: build detail + restore ──
	case 2:
		b := m.historyDetail
		if b == nil {
			return "No build selected."
		}
		lbl := lipgloss.NewStyle().Foreground(colorSubtext)
		val := lipgloss.NewStyle().Foreground(colorText)
		rows := []string{
			titleSt.Render("Build Detail"),
			sep,
			lbl.Render("ID:       ") + val.Render(b.ID),
			lbl.Render("App ID:   ") + val.Render(b.AppID),
			lbl.Render("Type:     ") + val.Render(b.BuildType),
			lbl.Render("Format:   ") + val.Render(b.Format),
			lbl.Render("Status:   ") + val.Render(buildStatusGlyph(b.Status)+" "+b.Status),
			lbl.Render("Created:  ") + val.Render(b.CreatedAt.Format(time.RFC1123)),
			lbl.Render("Artifact: ") + val.Render(b.ArtifactPath),
			"",
		}
		if m.historyRestoreStatus != "" {
			color := colorSuccess
			if strings.HasPrefix(m.historyRestoreStatus, "[ERR]") {
				color = colorError
			}
			rows = append(rows,
				lipgloss.NewStyle().Foreground(color).Bold(true).
					Render("  "+m.historyRestoreStatus))
			rows = append(rows, "")
		}
		restoreSt := lipgloss.NewStyle().
			Foreground(colorHighFg).Background(colorPrimary).
			Padding(0, 2).Bold(true)
		rows = append(rows, "  "+restoreSt.Render("[ Restore APK to ./"+b.ID+"/ ]"))
		rows = append(rows, "",
			lbl.Render("--- Log (last 10 lines) ---"))
		logLines := strings.Split(b.Log, "\n")
		if len(logLines) > 10 {
			logLines = logLines[len(logLines)-10:]
		}
		for _, l := range logLines {
			rows = append(rows, lipgloss.NewStyle().Foreground(colorSubtext).Render(l))
		}
		rows = append(rows, "",
			muteSt.Render("  [Enter/r] restore   [Esc/q] back"))
		return lipgloss.JoinVertical(lipgloss.Left, rows...)
	}
	return ""
}

// ─── Share tab ────────────────────────────────────────────────────────────────

func (m model) viewShareTab(w, h int) string {
	titleSt := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	subSt := lipgloss.NewStyle().Foreground(colorSubtext).PaddingLeft(1)
	selSt := lipgloss.NewStyle().Foreground(colorText).Background(colorHighBg).
		PaddingLeft(1).Bold(true)
	muteSt := lipgloss.NewStyle().Foreground(colorMuted)
	sep := strings.Repeat("-", w-6)

	switch m.shareState {
	case SharePickBuild:
		rows := []string{titleSt.Render("Share APK"), sep, ""}
		if len(m.shareBuilds) == 0 {
			rows = append(rows, subSt.Render("No successful builds yet."),
				subSt.Render("Build an APK first, then come back here."))
		}
		for i, b := range m.shareBuilds {
			ts := b.CreatedAt.Format("01-02 15:04")
			label := fmt.Sprintf("  %-10s  %-3s  %s  %s",
				b.BuildType, b.Format[:min3(b.Format)], ts, shortPath(b.ArtifactPath))
			if i == m.shareCursor && m.focus == FocusContent {
				rows = append(rows, selSt.Render(label))
			} else {
				rows = append(rows, subSt.Render(label))
			}
		}
		rows = append(rows, "", muteSt.Render("  [up/down] navigate   [Enter] select   [r] refresh"))
		return lipgloss.JoinVertical(lipgloss.Left, rows...)

	case SharePickMethod:
		return m.viewPrompt(w)

	case ShareServing:
		urlSt := lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
		rows := []string{
			titleSt.Render("Local WiFi Share"),
			sep,
			"",
			subSt.Render("Make sure your phone is on the same WiFi, then open:"),
			"",
			"  " + urlSt.Render(m.shareURL),
			"",
			"  " + m.qrCodeBlock(m.shareURL, w-6),
			"",
			lipgloss.NewStyle().Foreground(colorWarning).Render("  " + m.shareMessage),
			"",
			muteSt.Render("  [q] stop server"),
		}
		return lipgloss.JoinVertical(lipgloss.Left, rows...)

	case ShareUploading:
		frames := []string{"|", "/", "-", "\\"}
		spin := frames[m.buildSpinner%len(frames)]
		rows := []string{
			titleSt.Render("Uploading to S3"),
			sep,
			"",
			lipgloss.NewStyle().Foreground(colorWarning).
				Render("  " + spin + " " + m.shareMessage),
		}
		return lipgloss.JoinVertical(lipgloss.Left, rows...)

	case ShareDone:
		urlSt := lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
		rows := []string{
			titleSt.Render("Share Complete"),
			sep,
			"",
		}
		if m.shareURL != "" {
			rows = append(rows,
				subSt.Render("Download URL:"),
				"  "+urlSt.Render(m.shareURL),
				"",
				"  "+m.qrCodeBlock(m.shareURL, w-6),
			)
		}
		rows = append(rows,
			"",
			lipgloss.NewStyle().Foreground(colorSuccess).Bold(true).
				Render("  "+m.shareMessage),
			"",
			muteSt.Render("  [Esc/q] back"),
		)
		return lipgloss.JoinVertical(lipgloss.Left, rows...)
	}
	return ""
}

// qrCodeBlock renders a QR code using GenerateHalfBlock (half-block Unicode)
// and prints the URL below it.
func (m model) qrCodeBlock(url string, _ int) string {
	var buf bytes.Buffer
	qrterminal.GenerateHalfBlock(url, qrterminal.M, &buf)
	qrText := strings.TrimRight(buf.String(), "\n")
	urlLine := lipgloss.NewStyle().Foreground(colorSuccess).Bold(true).Render(url)
	return lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.NewStyle().Foreground(colorText).Render(qrText),
		"",
		urlLine,
	)
}

// ─── Settings tab ─────────────────────────────────────────────────────────────

func (m model) viewSettingsTab(w int) string {
	titleSt := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	lblSt := lipgloss.NewStyle().Foreground(colorSubtext).Width(32)
	valSt := lipgloss.NewStyle().Foreground(colorText)
	cursorSt := lipgloss.NewStyle().Foreground(colorText).Background(colorHighBg).Bold(true)
	editSt := lipgloss.NewStyle().Foreground(colorHighFg).Background(colorPrimary).Bold(true)
	saveSt := lipgloss.NewStyle().Foreground(colorHighFg).Background(colorPrimary).
		Padding(0, 2).Bold(true)
	savedSt := lipgloss.NewStyle().Foreground(colorHighFg).Background(colorSuccess).
		Padding(0, 2).Bold(true)
	muteSt := lipgloss.NewStyle().Foreground(colorMuted)
	maskHintSt := lipgloss.NewStyle().Foreground(colorWarning)

	rows := []string{
		titleSt.Render("Settings"),
		strings.Repeat("-", w-4), "",
	}

	for i := SettingsField(0); i < sfCount; i++ {
		isCursor := i == m.settingsCursor && m.focus == FocusContent
		isEditing := isCursor && m.settingsEditing
		isSensitive := sensitiveField(i)
		isUnmasked := m.settingsUnmask[i]

		if i == sfSave {
			lbl := "[ Save Settings ]"
			if m.settingsSaved {
				rows = append(rows, "  "+savedSt.Render("  Saved!  "))
			} else if isCursor {
				rows = append(rows, "  "+editSt.Render("> "+lbl))
			} else {
				rows = append(rows, "  "+saveSt.Render(lbl))
			}
			continue
		}

		rawVal := m.settingsFields[i]
		displayVal := rawVal
		if isSensitive && !isUnmasked && !isEditing {
			displayVal = maskSecret(rawVal)
		}
		if isEditing {
			displayVal += "_"
		}

		var row string
		if isEditing {
			row = "  " + lblSt.Render(i.Label()+":") + " " + editSt.Render(displayVal)
		} else if isCursor {
			row = "  " + cursorSt.Render("> "+i.Label()+":") + " " + valSt.Render(displayVal)
			if isSensitive {
				hint := " [m=unmask]"
				if isUnmasked {
					hint = " [m=mask]"
				}
				row += maskHintSt.Render(hint)
			}
		} else {
			row = "  " + lblSt.Render(i.Label()+":") + " " + valSt.Render(displayVal)
		}
		rows = append(rows, row)
	}

	rows = append(rows, "",
		muteSt.Render("  [up/down] navigate   [Enter] edit   [Esc] cancel   [m] toggle mask"))
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// ─── Header ───────────────────────────────────────────────────────────────────

func (m model) buildHeader() (string, int) {
	innerWidth := max(0, m.width-6)
	engine := gobanner.NewBannerEngine()
	cfg := gobanner.BannerConfig{
		TextColor:     gobanner.ColorRGB(124, 58, 237),
		AccentColor:   gobanner.ColorRGB(167, 139, 250),
		DividerBottom: true,
		DividerChar:   "-",
		Subtext:       "Expo Build Pipeline",
		SubtextPrefix: " ",
		UpperCase:     true,
	}
	title := string(engine.Generate("expo build", cfg))
	titleWidth := lipgloss.Width(title)
	remaining := max(0, innerWidth-titleWidth)
	ver := lipgloss.NewStyle().Width(remaining).Align(lipgloss.Right).
		Foreground(colorMuted).Render("v2.0.0 TUI")
	headerRow := lipgloss.JoinHorizontal(lipgloss.Center, title, ver)
	header := lipgloss.NewStyle().
		Width(innerWidth).
		Border(lipgloss.NormalBorder(), false, false, true, false).
		BorderForeground(colorBorder).Render(headerRow)
	return header, lipgloss.Height(header)
}

// ─── Viewport helpers ─────────────────────────────────────────────────────────

func (m *model) syncViewportSize() {
	if !m.ready || m.width == 0 || m.height == 0 {
		return
	}
	inner := max(0, m.width-6)
	sidebarWidth := 22
	contentWidth := max(0, inner-sidebarWidth-1)
	innerH := max(0, m.height-m.headerHeight-4)
	vpW := max(0, contentWidth-10)
	vpH := max(0, innerH-4)
	m.buildViewport.SetWidth(vpW)
	m.buildViewport.SetHeight(vpH)
}

func (m model) vpHeader() string {
	b := lipgloss.RoundedBorder()
	b.Right = "+"
	ts := lipgloss.NewStyle().BorderStyle(b).Padding(0, 1)
	lineColor := colorBorder
	if m.focus == FocusContent {
		ts = ts.BorderForeground(colorPrimary).Foreground(colorAccent)
		lineColor = colorPrimary
	}
	label := "build logs"
	if m.buildState == StateDone {
		label = "build logs (done)"
	} else if m.buildState == StateBuilding {
		frames := []string{"|", "/", "-", "\\"}
		label = frames[m.buildSpinner%len(frames)] + " building..."
	}
	title := ts.Render(label)
	lineLen := max(0, m.buildViewport.Width()-lipgloss.Width(title))
	line := lipgloss.NewStyle().Foreground(lineColor).Render(strings.Repeat("-", lineLen))
	return lipgloss.JoinHorizontal(lipgloss.Center, title, line)
}

func (m model) vpFooter() string {
	b := lipgloss.RoundedBorder()
	b.Left = "+"
	is := lipgloss.NewStyle().BorderStyle(b).Padding(0, 1)
	lineColor := colorBorder
	if m.focus == FocusContent {
		is = is.BorderForeground(colorPrimary).Foreground(colorAccent)
		lineColor = colorPrimary
	}
	pct := is.Render(fmt.Sprintf("%3.f%%", m.buildViewport.ScrollPercent()*100))
	lineLen := max(0, m.buildViewport.Width()-lipgloss.Width(pct))
	line := lipgloss.NewStyle().Foreground(lineColor).Render(strings.Repeat("-", lineLen))
	return lipgloss.JoinHorizontal(lipgloss.Center, line, pct)
}

// ─── Local HTTP server ────────────────────────────────────────────────────────

// serveAPKLocal starts a one-shot HTTP server, returns the URL and a stop func.
// The done signal (localServerDoneMsg) is NOT sent from here; the caller is
// responsible for sending it via tea.Cmd.
func serveAPKLocal(apkPath string) (string, func(), error) {
	info, err := os.Stat(apkPath)
	if err != nil {
		return "", nil, fmt.Errorf("APK not found: %w", err)
	}
	localIP, err := outboundIP()
	if err != nil {
		return "", nil, fmt.Errorf("cannot get local IP: %w", err)
	}
	listener, err := net.Listen("tcp", localIP+":0")
	if err != nil {
		return "", nil, fmt.Errorf("cannot bind port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	filename := filepath.Base(apkPath)
	url := fmt.Sprintf("http://%s:%d/%s", localIP, port, filename)

	done := make(chan struct{})
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("/"+filename, func(w http.ResponseWriter, r *http.Request) {
		f, ferr := os.Open(apkPath)
		if ferr != nil {
			http.Error(w, "unavailable", 500)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		w.Header().Set("Content-Type", "application/vnd.android.package-archive")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
		http.ServeContent(w, r, filename, time.Time{}, f)
		once.Do(func() { close(done) })
	})

	srv := &http.Server{Handler: mux}
	go func() {
		<-done
		time.Sleep(500 * time.Millisecond)
		srv.Shutdown(context.Background())
	}()
	go func() { srv.Serve(listener) }()

	stopFn := func() {
		srv.Shutdown(context.Background())
	}
	return url, stopFn, nil
}

func uploadToS3(apkPath string, s db.Settings) (string, error) {
	// S3 upload requires the AWS SDK; implementation deferred.
	// Callers should check credentials before invoking.
	return "", fmt.Errorf("S3 upload: please configure credentials in Settings and rebuild")
}

func outboundIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// ─── App discovery (monorepo) ─────────────────────────────────────────────────

// findExpoApps walks root looking for app.json files that contain an "expo"
// key. Returns relative paths; returns ["."] for a single-root project.
func findExpoApps(root string) []string {
	skipDirs := map[string]bool{
		"node_modules": true, "android": true, "ios": true,
		"build": true, "dist": true, "Pods": true,
	}
	var found []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (skipDirs[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "app.json" {
			data, e := os.ReadFile(path)
			if e != nil {
				return nil
			}
			if strings.Contains(string(data), `"expo"`) {
				rel, _ := filepath.Rel(root, filepath.Dir(path))
				if rel == "" {
					rel = "."
				}
				found = append(found, rel)
			}
		}
		return nil
	})
	if len(found) == 0 {
		return []string{"."}
	}
	return found
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func buildStatusGlyph(s string) string {
	switch s {
	case "success":
		return lipgloss.NewStyle().Foreground(colorSuccess).Render("[OK]")
	case "failed":
		return lipgloss.NewStyle().Foreground(colorError).Render("[ER]")
	case "running":
		return lipgloss.NewStyle().Foreground(colorWarning).Render("[..]")
	}
	return "[??]"
}

func maskSecret(s string) string {
	if len(s) == 0 {
		return ""
	}
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func parseBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min3(s string) int {
	if len(s) < 3 {
		return len(s)
	}
	return 3
}

func shortPath(p string) string {
	base := filepath.Base(p)
	if len(base) > 20 {
		return "..." + base[len(base)-17:]
	}
	return base
}

// ─── Cmd helpers ──────────────────────────────────────────────────────────────

func loadAppIDs(database *db.DB) tea.Cmd {
	return func() tea.Msg {
		ids, _ := database.ListAppIDs()
		return appIDsLoadedMsg{ids: ids}
	}
}

func loadBuildsForApp(database *db.DB, appID string) tea.Cmd {
	return func() tea.Msg {
		builds, _ := database.ListBuilds(appID)
		return historyLoadedMsg{builds: builds}
	}
}

func loadShareBuilds(database *db.DB) tea.Cmd {
	return func() tea.Msg {
		builds, _ := database.ListSuccessfulBuilds()
		return shareBuildsLoadedMsg{builds: builds}
	}
}

// ─── Helpers (project / app ID) ───────────────────────────────────────────────

func projectDir() string {
	dir, _ := os.Getwd()
	return dir
}

func resolveAppID(dir string) string {
	tomlPath := filepath.Join(dir, "expo-build.toml")
	if _, err := os.Stat(tomlPath); os.IsNotExist(err) {
		id := fmt.Sprintf("app-%d", time.Now().UnixNano())
		_ = os.WriteFile(tomlPath, []byte("[app]\napp_id = \""+id+"\"\n"), 0644)
		return id
	}
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return fmt.Sprintf("app-%d", time.Now().UnixNano())
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "app_id") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.Trim(strings.TrimSpace(parts[1]), "\"")
			}
		}
	}
	return fmt.Sprintf("app-%d", time.Now().UnixNano())
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	database, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "expo-build: failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	zone.NewGlobal()
	defer zone.Close()

	appID := resolveAppID(projectDir())

	p := tea.NewProgram(initialModel(database, appID))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nexpo-build exited: %v\n", err)
		os.Exit(1)
	}
}
