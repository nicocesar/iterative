package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web/index.html
var indexHTML []byte

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

func updateVersionFile(n int) error {
	versionContent := fmt.Sprintf("{\"iteration\":\"%d\"}\n", n)
	versionPath := "frontend/version.json"
	
	// Ensure frontend directory exists
	if err := os.MkdirAll("frontend", 0o755); err != nil {
		return fmt.Errorf("failed to create frontend directory: %v", err)
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

func executePostSaveCommands(n int) error {
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
	if err := updateVersionFile(n); err != nil {
		endTime := time.Now()
		stats.EndTime = endTime.Format(time.RFC3339)
		stats.Duration = time.Since(startTime).Seconds()
		stats.ActualCost = actualCost
		stats.Error = fmt.Sprintf("Failed to update version file: %v", err)
		
		saveExecutionStats(stats)
		
		broadcastMessage("error", fmt.Sprintf("Failed to update version file: %v", err))
		return fmt.Errorf("failed to update version file: %v", err)
	}
	
	// Add all files in frontend/src directory (new and modified)
	if _, err := os.Stat("frontend/src"); err == nil {
		gitAddCmd := exec.Command("git", "add", "frontend/src/")
		addOutput, err := gitAddCmd.CombinedOutput()
		if err != nil {
			// Log but don't fail - might be no frontend/src changes
			log.Printf("Git add frontend/src warning: %v, output: %s", err, string(addOutput))
		}
	}
	
	// Add captures directory and version file explicitly
	gitAddCapturesCmd := exec.Command("git", "add", "frontend/version.json")
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

func handleSave(w http.ResponseWriter, r *http.Request) {
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


	// Execute post-save commands (Claude Code + git commit)
	go func() {
		if err := executePostSaveCommands(n); err != nil {
			log.Printf("Post-save commands failed: %v", err)
		} else {
			log.Printf("Post-save commands completed successfully for iteration-%s", pad3(n))
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(saveResp{N: n})
}

func main() {
	// Ensure we can write to captures dir
	if err := os.MkdirAll(capturesDir, fs.FileMode(0o755)); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/ws", handleWebSocket)
	mux.HandleFunc("/next", handleNext)
	mux.HandleFunc("/save", handleSave)

	addr := ":8787"
	log.Printf("Nico Cesar's iterative running at http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
