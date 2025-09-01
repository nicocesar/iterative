package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed version.txt
var version string

type Config struct {
	Version     string `json:"version" mapstructure:"version"`
	Target      string `json:"target" mapstructure:"target"`
	Cmd         string `json:"cmd" mapstructure:"cmd"`
	URL         string `json:"url" mapstructure:"url"`
	ForceReload bool   `json:"forceReload" mapstructure:"forceReload"`
}

var (
	capturesDir = "captures"
	counterMu   sync.Mutex
	claudePrompt = "take a look into @captures/  each iteration will make you do something, the txt file is the png is the screen capture with red rectangles to pay attention to. Choose the latest iteration always"
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow connections from any origin
		},
	}
	clients = make(map[*websocket.Conn]bool)
	clientsMu sync.Mutex
)

type ProcessManager struct {
	cmd           *exec.Cmd
	currentLogNum int
	logFile       *os.File
	mu            sync.Mutex
	running       bool
	config        *Config
}

type IterativeServer struct {
	config         *Config
	processManager *ProcessManager
}

type wsMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type executionStats struct {
	Iteration    int     `json:"iteration"`
	StartTime    string  `json:"start_time"`
	EndTime      string  `json:"end_time"`
	Duration     float64 `json:"duration_seconds"`
	ActualCost   float64 `json:"actual_cost_usd,omitempty"`
	Success      bool    `json:"success"`
	Error        string  `json:"error,omitempty"`
}

func nextNumber() (int, error) {
	if _, err := os.Stat(capturesDir); os.IsNotExist(err) {
		if err := os.MkdirAll(capturesDir, 0o755); err != nil {
			return 0, err
		}
	}
	entries, err := os.ReadDir(capturesDir)
	if err != nil {
		return 0, err
	}
	re := regexp.MustCompile(`^iteration-(\d{3})\.png$`)
	var nums []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := re.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return 1, nil
	}
	sort.Ints(nums)
	return nums[len(nums)-1] + 1, nil
}

func pad3(n int) string { return fmt.Sprintf("%03d", n) }

func updateVersionFile(n int, targetDir string) error {
	versionContent := fmt.Sprintf("{\"iteration\":\"%d\"}\n", n)
	versionPath := filepath.Join(targetDir, "version.json")
	
	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("failed to create target directory: %v", err)
	}
	
	if err := os.WriteFile(versionPath, []byte(versionContent), 0o644); err != nil {
		return fmt.Errorf("failed to write version file: %v", err)
	}
	
	return nil
}

func saveExecutionStats(stats executionStats) error {
	statsPath := filepath.Join(capturesDir, fmt.Sprintf("iteration-%s.json", pad3(stats.Iteration)))
	
	statsJSON, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal stats: %v", err)
	}
	
	if err := os.WriteFile(statsPath, statsJSON, 0o644); err != nil {
		return fmt.Errorf("failed to write stats file: %v", err)
	}
	
	return nil
}

type ccusageSession struct {
	Cost        float64 `json:"cost"`
	InputTokens int     `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	CacheReadTokens int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

type ccusageResponse struct {
	Sessions []ccusageSession `json:"sessions"`
}

func getRealCostData() (float64, error) {
	// Option C: Read Claude Code JSONL files directly to get session-specific costs
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return 0, fmt.Errorf("failed to get home directory: %v", err)
	}
	
	// Current working directory path encoded as Claude project directory
	wd, _ := os.Getwd()
	claudeProjectDir := filepath.Join(homeDir, ".claude", "projects", strings.ReplaceAll(wd, "/", "-"))
	log.Printf("Looking for Claude project sessions in: %s", claudeProjectDir)
	
	// Find the most recent JSONL file (most recent session)
	sessionFile, err := findMostRecentSession(claudeProjectDir)
	if err != nil {
		log.Printf("Failed to find recent session, falling back to ccusage daily")
		return fallbackToCcusageDaily()
	}
	
	log.Printf("Found most recent session file: %s", sessionFile)
	
	// Parse the JSONL file to extract token usage
	tokenUsage, err := parseSessionTokens(sessionFile)
	if err != nil {
		log.Printf("Failed to parse session tokens: %v, falling back to ccusage daily", err)
		return fallbackToCcusageDaily()
	}
	
	// Calculate cost based on current Sonnet 4 pricing
	cost := calculateSessionCost(tokenUsage)
	log.Printf("Calculated session cost: $%.4f (input: %d, output: %d, cache_read: %d, cache_write: %d)", 
		cost, tokenUsage.InputTokens, tokenUsage.OutputTokens, tokenUsage.CacheReadTokens, tokenUsage.CacheWriteTokens)
	
	return cost, nil
}

type sessionTokenUsage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

func findMostRecentSession(claudeDir string) (string, error) {
	entries, err := os.ReadDir(claudeDir)
	if err != nil {
		return "", fmt.Errorf("failed to read Claude project directory: %v", err)
	}
	
	var mostRecent string
	var mostRecentTime time.Time
	
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			fullPath := filepath.Join(claudeDir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.ModTime().After(mostRecentTime) {
				mostRecentTime = info.ModTime()
				mostRecent = fullPath
			}
		}
	}
	
	if mostRecent == "" {
		return "", fmt.Errorf("no JSONL session files found")
	}
	
	return mostRecent, nil
}

func parseSessionTokens(sessionFile string) (sessionTokenUsage, error) {
	file, err := os.Open(sessionFile)
	if err != nil {
		return sessionTokenUsage{}, fmt.Errorf("failed to open session file: %v", err)
	}
	defer file.Close()
	
	var usage sessionTokenUsage
	scanner := bufio.NewScanner(file)
	
	// Increase buffer size for large JSONL lines (Claude Code can have very long lines)
	const maxCapacity = 1024 * 1024 // 1MB buffer instead of default 64KB
	buf := make([]byte, maxCapacity)
	scanner.Buffer(buf, maxCapacity)
	
	lineCount := 0
	tokensFoundInLines := 0
	
	for scanner.Scan() {
		lineCount++
		line := scanner.Bytes()
		
		var entry map[string]interface{}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		
		foundTokensThisLine := false
		
		// Look for token usage in message.usage (Claude Code format)
		if message, ok := entry["message"].(map[string]interface{}); ok {
			if tokens, ok := message["usage"].(map[string]interface{}); ok {
				if input, ok := tokens["input_tokens"].(float64); ok {
					usage.InputTokens += int(input)
					foundTokensThisLine = true
				}
				if output, ok := tokens["output_tokens"].(float64); ok {
					usage.OutputTokens += int(output)
					foundTokensThisLine = true
				}
				if cacheRead, ok := tokens["cache_read_input_tokens"].(float64); ok {
					usage.CacheReadTokens += int(cacheRead)
					foundTokensThisLine = true
				}
				if cacheWrite, ok := tokens["cache_creation_input_tokens"].(float64); ok {
					usage.CacheWriteTokens += int(cacheWrite)
					foundTokensThisLine = true
				}
			}
		}
		
		// Look for token usage in top-level usage (fallback)
		if tokens, ok := entry["usage"].(map[string]interface{}); ok {
			if input, ok := tokens["input_tokens"].(float64); ok {
				usage.InputTokens += int(input)
				foundTokensThisLine = true
			}
			if output, ok := tokens["output_tokens"].(float64); ok {
				usage.OutputTokens += int(output)
				foundTokensThisLine = true
			}
			if cacheRead, ok := tokens["cache_read_tokens"].(float64); ok {
				usage.CacheReadTokens += int(cacheRead)
				foundTokensThisLine = true
			}
			if cacheWrite, ok := tokens["cache_creation_tokens"].(float64); ok {
				usage.CacheWriteTokens += int(cacheWrite)
				foundTokensThisLine = true
			}
		}
		
		// Also check top-level token fields
		if input, ok := entry["input_tokens"].(float64); ok {
			usage.InputTokens += int(input)
			foundTokensThisLine = true
		}
		if output, ok := entry["output_tokens"].(float64); ok {
			usage.OutputTokens += int(output)
			foundTokensThisLine = true
		}
		
		if foundTokensThisLine {
			tokensFoundInLines++
		}
	}
	
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return usage, fmt.Errorf("no token usage found in session file")
	}
	
	if scanErr := scanner.Err(); scanErr != nil {
		return usage, scanErr
	}
	
	return usage, nil
}

func calculateSessionCost(usage sessionTokenUsage) float64 {
	// Current Sonnet 4 pricing (as of late 2024/early 2025)
	// These rates should be updated if pricing changes
	const (
		inputRate       = 3.00  / 1000000  // $3 per million input tokens
		outputRate      = 15.00 / 1000000  // $15 per million output tokens  
		cacheWriteRate  = 3.75  / 1000000  // $3.75 per million cache write tokens
		cacheReadRate   = 0.30  / 1000000  // $0.30 per million cache read tokens
	)
	
	totalCost := float64(usage.InputTokens)*inputRate +
		float64(usage.OutputTokens)*outputRate +
		float64(usage.CacheWriteTokens)*cacheWriteRate +
		float64(usage.CacheReadTokens)*cacheReadRate
		
	return totalCost
}

func fallbackToCcusageDaily() (float64, error) {
	log.Printf("Using fallback: ccusage daily report")
	today := time.Now().Format("20060102")
	ccusageCmd := exec.Command("npx", "ccusage@latest", "daily", "--json", "--since", today)
	output, err := ccusageCmd.CombinedOutput()
	
	if err != nil {
		return 0, fmt.Errorf("ccusage fallback failed: %v", err)
	}
	
	var dailyResponse map[string]interface{}
	if err := json.Unmarshal(output, &dailyResponse); err != nil {
		return 0, fmt.Errorf("failed to parse ccusage fallback: %v", err)
	}
	
	cost := extractCostFromResponse(dailyResponse)
	if cost <= 0 {
		return 0, fmt.Errorf("no valid cost found in fallback")
	}
	
	return cost, nil
}

func getKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func extractCostFromResponse(response map[string]interface{}) float64 {
	// Try various possible locations for cost data
	if totalCost, ok := response["totalCost"].(float64); ok {
		return totalCost
	}
	if cost, ok := response["cost"].(float64); ok {
		return cost
	}
	if totals, ok := response["totals"].(map[string]interface{}); ok {
		if totalCost, ok := totals["totalCost"].(float64); ok {
			return totalCost
		}
		if cost, ok := totals["cost"].(float64); ok {
			return cost
		}
	}
	// Try looking in daily data array
	if daily, ok := response["daily"].([]interface{}); ok && len(daily) > 0 {
		if entry, ok := daily[len(daily)-1].(map[string]interface{}); ok {
			if cost, ok := entry["cost"].(float64); ok {
				return cost
			}
			if totalCost, ok := entry["totalCost"].(float64); ok {
				return totalCost
			}
		}
	}
	return 0
}

func broadcastMessage(msgType, message string) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	
	msg := wsMessage{Type: msgType, Message: message}
	data, _ := json.Marshal(msg)
	
	for client := range clients {
		if err := client.WriteMessage(websocket.TextMessage, data); err != nil {
			client.Close()
			delete(clients, client)
		}
	}
}

func (pm *ProcessManager) startProcess() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pm.running {
		return nil
	}

	// Get the current iteration number for the log file
	iterNum, err := nextNumber()
	if err != nil {
		return fmt.Errorf("failed to get iteration number: %v", err)
	}
	pm.currentLogNum = iterNum

	// Create log file
	logPath := filepath.Join(capturesDir, fmt.Sprintf("iteration-%s.log", pad3(pm.currentLogNum)))
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to create log file %s: %v", logPath, err)
	}
	pm.logFile = logFile

	// Parse the command (handle shell commands with arguments)
	cmdParts := strings.Fields(pm.config.Cmd)
	if len(cmdParts) == 0 {
		return fmt.Errorf("empty command")
	}

	// Create the command
	pm.cmd = exec.Command(cmdParts[0], cmdParts[1:]...)
	pm.cmd.Stdout = logFile
	pm.cmd.Stderr = logFile

	// Start the process
	if err := pm.cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start command '%s': %v", pm.config.Cmd, err)
	}

	pm.running = true
	
	log.Printf("Started process: %s (PID: %d) -> %s", pm.config.Cmd, pm.cmd.Process.Pid, logPath)
	broadcastMessage("process", fmt.Sprintf("✅ Started: %s (PID: %d)", pm.config.Cmd, pm.cmd.Process.Pid))

	// Monitor the process in a goroutine
	go pm.monitorProcess()

	return nil
}

func (pm *ProcessManager) monitorProcess() {
	if pm.cmd == nil || pm.cmd.Process == nil {
		return
	}

	err := pm.cmd.Wait()
	
	pm.mu.Lock()
	defer pm.mu.Unlock()
	
	pm.running = false
	if pm.logFile != nil {
		pm.logFile.Close()
		pm.logFile = nil
	}

	if err != nil {
		log.Printf("Process '%s' (PID: %d) exited with error: %v", pm.config.Cmd, pm.cmd.Process.Pid, err)
		broadcastMessage("process", fmt.Sprintf("❌ Process crashed: %s (PID: %d) - %v", pm.config.Cmd, pm.cmd.Process.Pid, err))
		
		// Auto-restart if not forceReload
		if !pm.config.ForceReload {
			log.Printf("Auto-restarting process...")
			broadcastMessage("process", "🔄 Auto-restarting process...")
			if restartErr := pm.startProcess(); restartErr != nil {
				log.Printf("Failed to restart process: %v", restartErr)
				broadcastMessage("process", fmt.Sprintf("❌ Failed to restart: %v", restartErr))
			}
		}
	} else {
		log.Printf("Process '%s' (PID: %d) exited normally", pm.config.Cmd, pm.cmd.Process.Pid)
		broadcastMessage("process", fmt.Sprintf("✅ Process exited normally: %s (PID: %d)", pm.config.Cmd, pm.cmd.Process.Pid))
	}
}

func (pm *ProcessManager) switchToNextLogFile() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.running {
		return nil
	}

	// Close current log file
	if pm.logFile != nil {
		pm.logFile.Close()
	}

	// Get next iteration number
	iterNum, err := nextNumber()
	if err != nil {
		return fmt.Errorf("failed to get next iteration number: %v", err)
	}
	pm.currentLogNum = iterNum

	// Create new log file
	logPath := filepath.Join(capturesDir, fmt.Sprintf("iteration-%s.log", pad3(pm.currentLogNum)))
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to create new log file %s: %v", logPath, err)
	}
	pm.logFile = logFile

	// Redirect process output to new log file
	// Note: This is tricky with running processes. We'll write a log entry about the switch.
	timestamp := time.Now().Format(time.RFC3339)
	switchMsg := fmt.Sprintf("\n[%s] === Switched to iteration-%s.log ===\n", timestamp, pad3(pm.currentLogNum))
	pm.logFile.WriteString(switchMsg)

	log.Printf("Switched process output to: %s", logPath)
	broadcastMessage("process", fmt.Sprintf("📝 Switched to log: iteration-%s.log", pad3(pm.currentLogNum)))

	return nil
}

func (pm *ProcessManager) restartProcess() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Stop current process if running
	if pm.running && pm.cmd != nil && pm.cmd.Process != nil {
		log.Printf("Stopping process (PID: %d) for restart", pm.cmd.Process.Pid)
		pm.cmd.Process.Kill()
		pm.cmd.Wait() // Wait for cleanup
		pm.running = false
	}

	// Close current log file
	if pm.logFile != nil {
		pm.logFile.Close()
		pm.logFile = nil
	}

	pm.mu.Unlock()
	// Start new process (this will handle its own locking)
	return pm.startProcess()
}

func (pm *ProcessManager) stop() error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !pm.running || pm.cmd == nil || pm.cmd.Process == nil {
		return nil
	}

	log.Printf("Stopping process (PID: %d)", pm.cmd.Process.Pid)
	broadcastMessage("process", fmt.Sprintf("🛑 Stopping process (PID: %d)", pm.cmd.Process.Pid))
	
	err := pm.cmd.Process.Kill()
	if err != nil {
		log.Printf("Failed to kill process: %v", err)
		return err
	}

	pm.cmd.Wait() // Wait for cleanup
	pm.running = false

	if pm.logFile != nil {
		pm.logFile.Close()
		pm.logFile = nil
	}

	return nil
}

func executePostSaveCommands(n int, config *Config) error {
	startTime := time.Now()
	stats := executionStats{
		Iteration: n,
		StartTime: startTime.Format(time.RFC3339),
		Success:   false,
	}

	broadcastMessage("status", fmt.Sprintf("🤖 Running iteration %s...", pad3(n)))
	
	claudeCmd := exec.Command("npx", "@anthropic-ai/claude-code", "--allowedTools", "Read", "--permission-mode", "acceptEdits", "-p", claudePrompt)
	output, err := claudeCmd.CombinedOutput()
	
	if err != nil {
		endTime := time.Now()
		stats.EndTime = endTime.Format(time.RFC3339)
		stats.Duration = time.Since(startTime).Seconds()
		stats.Error = fmt.Sprintf("Claude command failed: %v", err)
		
		saveExecutionStats(stats) // Save even if failed
		
		errorMsg := fmt.Sprintf("Claude command failed: %v\nOutput: %s", err, string(output))
		log.Printf("Claude command error details: %s", errorMsg)
		broadcastMessage("error", fmt.Sprintf("Claude command failed: %v\n%s", err, string(output)))
		return fmt.Errorf("claude command failed: %v, output: %s", err, output)
	}

	commitMessage := strings.TrimSpace(string(output))
	if commitMessage == "" {
		commitMessage = "Automated commit after save"
	}

	broadcastMessage("status", "📝 Committing changes...")
	
	// Get real cost data from ccusage
	broadcastMessage("status", "💰 Getting real cost data...")
	
	// Debug: Check for Claude Code directories and files
	homeDir, _ := os.UserHomeDir()
	claudeDirs := []string{
		filepath.Join(homeDir, ".claude"),
		filepath.Join(homeDir, ".config", "claude"),
		filepath.Join(homeDir, ".claude", "projects"),
	}
	
	for _, dir := range claudeDirs {
		if stat, err := os.Stat(dir); err == nil {
			if stat.IsDir() {
				log.Printf("Found Claude directory: %s", dir)
				// List contents
				entries, err := os.ReadDir(dir)
				if err == nil {
					log.Printf("Contents of %s:", dir)
					for _, entry := range entries {
						log.Printf("  - %s", entry.Name())
					}
				}
			}
		} else {
			log.Printf("Claude directory not found: %s", dir)
		}
	}
	
	actualCost, costErr := getRealCostData()
	if costErr != nil {
		log.Printf("Failed to get real cost data: %v", costErr)
		// Continue without cost data rather than failing
		actualCost = 0
	}

	// Update version file right before commit
	if err := updateVersionFile(n, config.Target); err != nil {
		endTime := time.Now()
		stats.EndTime = endTime.Format(time.RFC3339)
		stats.Duration = time.Since(startTime).Seconds()
		stats.ActualCost = actualCost
		stats.Error = fmt.Sprintf("Failed to update version file: %v", err)
		
		saveExecutionStats(stats)
		
		broadcastMessage("error", fmt.Sprintf("Failed to update version file: %v", err))
		return fmt.Errorf("failed to update version file: %v", err)
	}
	
	// Add all files in target/src directory (new and modified)
	targetSrcDir := filepath.Join(config.Target, "src")
	if _, err := os.Stat(targetSrcDir); err == nil {
		gitAddCmd := exec.Command("git", "add", targetSrcDir+"/")
		addOutput, err := gitAddCmd.CombinedOutput()
		if err != nil {
			// Log but don't fail - might be no target/src changes
			log.Printf("Git add %s warning: %v, output: %s", targetSrcDir, err, string(addOutput))
		}
	}
	
	// Add captures directory and version file explicitly
	versionFilePath := filepath.Join(config.Target, "version.json")
	gitAddCapturesCmd := exec.Command("git", "add", versionFilePath)
	capturesOutput, err := gitAddCapturesCmd.CombinedOutput()
	if err != nil {
		log.Printf("Git add captures warning: %v, output: %s", err, string(capturesOutput))
		// Continue anyway - files might already be staged
	}
	
	// Include execution stats in commit message
	endTime := time.Now()
	totalDuration := time.Since(startTime).Seconds()
	
	var enhancedCommitMessage string
	if actualCost > 0 {
		enhancedCommitMessage = fmt.Sprintf("%s\n\n[Execution Stats: %.1fs, $%.4f]", 
			commitMessage, totalDuration, actualCost)
	} else {
		enhancedCommitMessage = fmt.Sprintf("%s\n\n[Execution Stats: %.1fs, cost unavailable]", 
			commitMessage, totalDuration)
	}
	
	gitCmd := exec.Command("git", "commit", "-m", enhancedCommitMessage)
	gitOutput, err := gitCmd.CombinedOutput()
	if err != nil {
		stats.EndTime = endTime.Format(time.RFC3339)
		stats.Duration = totalDuration
		stats.ActualCost = actualCost
		stats.Error = fmt.Sprintf("Git commit failed: %v", err)
		
		saveExecutionStats(stats)
		
		errorMsg := fmt.Sprintf("Git commit failed: %v\nOutput: %s", err, string(gitOutput))
		log.Printf("Git commit error details: %s", errorMsg)
		broadcastMessage("error", fmt.Sprintf("Git commit failed: %v\n%s", err, string(gitOutput)))
		return fmt.Errorf("git commit failed: %v, output: %s", err, string(gitOutput))
	}

	// Save successful execution stats
	stats.EndTime = endTime.Format(time.RFC3339)
	stats.Duration = totalDuration
	stats.ActualCost = actualCost
	stats.Success = true
	
	if err := saveExecutionStats(stats); err != nil {
		log.Printf("Failed to save execution stats: %v", err)
	}

	broadcastMessage("success", fmt.Sprintf("✅ Committed: %s", commitMessage))
	
	// Send stats update to frontend
	var statsMsg string
	if actualCost > 0 {
		statsMsg = fmt.Sprintf("📊 Duration: %.1fs, Cost: $%.4f", totalDuration, actualCost)
	} else {
		statsMsg = fmt.Sprintf("📊 Duration: %.1fs, Cost: unavailable", totalDuration)
	}
	broadcastMessage("stats", statsMsg)
	
	return nil
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	clientsMu.Lock()
	clients[conn] = true
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, conn)
		clientsMu.Unlock()
	}()

	// Keep connection alive
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Now(), bytes.NewReader(indexHTML))
}

type nextResp struct {
	Next int `json:"next"`
}

func handleNext(w http.ResponseWriter, r *http.Request) {
	counterMu.Lock()
	defer counterMu.Unlock()
	n, err := nextNumber()
	if err != nil {
		http.Error(w, "failed to compute next", 500)
		return
	}
	json.NewEncoder(w).Encode(nextResp{Next: n})
}

type saveReq struct {
	N     *int   `json:"n,omitempty"`     // optional: client-supplied chosen number
	Image string `json:"imageDataURL"`    // "data:image/png;base64,...."
	Text  string `json:"text,omitempty"`  // notes
}
type saveResp struct {
	N int `json:"n"`
}

func (s *IterativeServer) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req saveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}

	counterMu.Lock()
	defer counterMu.Unlock()

	// Choose N
	n := 0
	if req.N != nil {
		n = *req.N
	} else {
		var err error
		n, err = nextNumber()
		if err != nil {
			http.Error(w, "cannot choose number", 500)
			return
		}
	}

	// If number is already taken (race), bump up to the next free
	for {
		pngPath := filepath.Join(capturesDir, fmt.Sprintf("iteration-%s.png", pad3(n)))
		if _, err := os.Stat(pngPath); os.IsNotExist(err) {
			break
		}
		n++
	}

	// Decode base64 image data
	prefix := "base64,"
	idx := strings.Index(req.Image, prefix)
	if idx < 0 {
		http.Error(w, "invalid image data", 400)
		return
	}
	raw := req.Image[idx+len(prefix):]
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		http.Error(w, "decode image failed", 400)
		return
	}

	// Write files
	pngPath := filepath.Join(capturesDir, fmt.Sprintf("iteration-%s.png", pad3(n)))
	txtPath := filepath.Join(capturesDir, fmt.Sprintf("iteration-%s.txt", pad3(n)))

	if err := os.WriteFile(pngPath, data, 0o644); err != nil {
		http.Error(w, "write png failed", 500)
		return
	}
	// Prepend a small header with timestamp for convenience
	header := fmt.Sprintf("[iteration-%s] %s\n\n", pad3(n), time.Now().Format(time.RFC3339))
	if err := os.WriteFile(txtPath, []byte(header+req.Text), 0o644); err != nil {
		_ = os.Remove(pngPath)
		http.Error(w, "write txt failed", 500)
		return
	}


	// Handle process management for new iteration
	go func() {
		// Handle forceReload logic
		if s.config.ForceReload {
			// Restart the process for each iteration
			if err := s.processManager.restartProcess(); err != nil {
				log.Printf("Failed to restart process for iteration: %v", err)
			}
		} else {
			// Just switch to next log file, keep process running
			if err := s.processManager.switchToNextLogFile(); err != nil {
				log.Printf("Failed to switch log file for iteration: %v", err)
			}
		}
		
		// Execute post-save commands (Claude Code + git commit)
		if err := executePostSaveCommands(n, s.config); err != nil {
			log.Printf("Post-save commands failed: %v", err)
		} else {
			log.Printf("Post-save commands completed successfully for iteration-%s", pad3(n))
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(saveResp{N: n})
}

func loadConfig(configDir string) (*Config, error) {
	configPath := filepath.Join(configDir, "iterative.config.json")
	
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("no iterative.config.json found in %s. Run 'iterative init' to create one", configDir)
	}
	
	viper.SetConfigName("iterative.config")
	viper.SetConfigType("json")
	viper.AddConfigPath(configDir)
	
	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config: %v", err)
	}
	
	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %v", err)
	}
	
	return &config, nil
}

func createDefaultConfig(dir string) error {
	configPath := filepath.Join(dir, "iterative.config.json")
	
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("iterative.config.json already exists in %s", dir)
	}
	
	defaultConfig := Config{
		Version:     "1",
		Target:      ".",
		Cmd:         "npm run dev",
		URL:         "http://localhost:5173",
		ForceReload: false,
	}
	
	configJSON, err := json.MarshalIndent(defaultConfig, "", "\t")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}
	
	if err := os.WriteFile(configPath, configJSON, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}
	
	fmt.Printf("Created iterative.config.json in %s\n", dir)
	return nil
}

func runServer(config *Config, targetDir string) error {
	// Change to target directory
	if err := os.Chdir(targetDir); err != nil {
		return fmt.Errorf("failed to change to target directory %s: %v", targetDir, err)
	}
	
	// Ensure we can write to captures dir
	if err := os.MkdirAll(capturesDir, fs.FileMode(0o755)); err != nil {
		return fmt.Errorf("failed to create captures directory: %v", err)
	}

	// Create process manager
	processManager := &ProcessManager{
		config: config,
	}

	server := &IterativeServer{
		config:         config,
		processManager: processManager,
	}
	
	// Start the initial process
	if err := processManager.startProcess(); err != nil {
		log.Printf("Failed to start initial process: %v", err)
		broadcastMessage("process", fmt.Sprintf("❌ Failed to start process: %v", err))
	}
	
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/ws", handleWebSocket)
	mux.HandleFunc("/next", handleNext)
	mux.HandleFunc("/save", server.handleSave)

	// Setup HTTP server with graceful shutdown
	httpServer := &http.Server{
		Addr:    ":8787",
		Handler: mux,
	}
	
	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	
	// Start server in goroutine
	serverErrChan := make(chan error, 1)
	go func() {
		log.Printf("Nico Cesar's iterative running at http://localhost:8787")
		log.Printf("Target directory: %s", targetDir)
		log.Printf("Config: cmd=%s, url=%s, forceReload=%t", config.Cmd, config.URL, config.ForceReload)
		serverErrChan <- httpServer.ListenAndServe()
	}()
	
	// Wait for shutdown signal or server error
	select {
	case err := <-serverErrChan:
		if err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	case sig := <-sigChan:
		log.Printf("Received signal %s, shutting down gracefully...", sig)
	}
	
	// Graceful shutdown
	log.Printf("Stopping process manager...")
	if err := processManager.stop(); err != nil {
		log.Printf("Error stopping process: %v", err)
	}
	
	log.Printf("Shutting down HTTP server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
	
	return nil
}

func performSanityChecks(targetDir string) error {
	log.Printf("Performing sanity checks...")
	
	// Check git installation
	gitCmd := exec.Command("git", "--version")
	gitOutput, err := gitCmd.Output()
	if err != nil {
		return fmt.Errorf("git is not installed or not available in PATH: %v", err)
	}
	gitVersion := strings.TrimSpace(string(gitOutput))
	log.Printf("✓ Git check passed: %s", gitVersion)
	
	// Check if target directory is a git repository
	gitDirCheck := exec.Command("git", "rev-parse", "--git-dir")
	gitDirCheck.Dir = targetDir
	_, err = gitDirCheck.Output()
	if err != nil {
		log.Printf("⚠ Target directory is not a git repository: %s", targetDir)
		
		// Prompt user to initialize git repository
		fmt.Print("Would you like to initialize a git repository? (y/N): ")
		var response string
		fmt.Scanln(&response)
		
		if strings.ToLower(response) == "y" || strings.ToLower(response) == "yes" {
			log.Printf("Initializing git repository in %s...", targetDir)
			gitInitCmd := exec.Command("git", "init")
			gitInitCmd.Dir = targetDir
			initOutput, initErr := gitInitCmd.CombinedOutput()
			if initErr != nil {
				return fmt.Errorf("failed to initialize git repository: %v\nOutput: %s", initErr, string(initOutput))
			}
			log.Printf("✓ Git repository initialized successfully")
		} else {
			return fmt.Errorf("git repository is required for iterative development (commits are made automatically)")
		}
	} else {
		log.Printf("✓ Git repository check passed")
	}
	
	// Check Claude Code availability
	claudeCmd := exec.Command("npx", "@anthropic-ai/claude-code", "--version")
	claudeOutput, err := claudeCmd.Output()
	if err != nil {
		return fmt.Errorf("Claude Code is not available (try: npm install -g @anthropic-ai/claude-code): %v", err)
	}
	claudeVersion := strings.TrimSpace(string(claudeOutput))
	log.Printf("✓ Claude Code check passed: %s", claudeVersion)
	
	log.Printf("All sanity checks passed!")
	return nil
}

func main() {
	var rootCmd = &cobra.Command{
		Use:     "iterative",
		Short:   "Iterative development tool with Claude Code integration",
		Version: strings.TrimSpace(version),
	}

	var serveCmd = &cobra.Command{
		Use:   "serve [directory]",
		Short: "Start the iterative server",
		Long:  "Start the iterative server. Directory parameter specifies where to look for iterative.config.json (default: current directory)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			configDir := "."
			if len(args) > 0 {
				configDir = args[0]
			}
			
			absConfigDir, err := filepath.Abs(configDir)
			if err != nil {
				return fmt.Errorf("failed to get absolute path for config directory: %v", err)
			}
			
			config, err := loadConfig(absConfigDir)
			if err != nil {
				return err
			}
			
			targetDir := config.Target
			if !filepath.IsAbs(targetDir) {
				targetDir = filepath.Join(absConfigDir, targetDir)
			}
			
			absTargetDir, err := filepath.Abs(targetDir)
			if err != nil {
				return fmt.Errorf("failed to get absolute path for target directory: %v", err)
			}
			
			// Perform sanity checks with the target directory
			if err := performSanityChecks(absTargetDir); err != nil {
				return fmt.Errorf("sanity check failed: %v", err)
			}
			
			return runServer(config, absTargetDir)
		},
	}

	var initCmd = &cobra.Command{
		Use:   "init [directory]",
		Short: "Initialize a new iterative project",
		Long:  "Create a new iterative.config.json file in the specified directory (default: current directory)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return fmt.Errorf("failed to get absolute path: %v", err)
			}
			
			return createDefaultConfig(absDir)
		},
	}

	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(initCmd)

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}
