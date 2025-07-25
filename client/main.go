package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/chzyer/readline"
	//	"github.com/davecgh/go-spew/spew"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// AnthropicParameters represents runtime parameters for Anthropic requests
type AnthropicParameters struct {
	MaxTokens     int      `json:"max_tokens"`               // Maximum number of tokens to generate
	Temperature   float64  `json:"temperature,omitempty"`    // Controls randomness (0.0 to 1.0)
	TopP          float64  `json:"top_p,omitempty"`          // Nucleus sampling (0.0 to 1.0)
	TopK          int      `json:"top_k,omitempty"`          // Top-k sampling
	StopSequences []string `json:"stop_sequences,omitempty"` // Stop sequences
}


// ModelDefinition represents the structure of a model definition file
type ModelDefinition struct {
	Name       string               `json:"name"`        // Claude model name
	Provider   string               `json:"provider"`    // "direct" or "bedrock"
	Region     string               `json:"region,omitempty"` // For Bedrock
	Parameters AnthropicParameters `json:"parameters"`
	System     string               `json:"system"`
	Format     string               `json:"format,omitempty"` // Optional response format
}

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AnthropicRequest represents a chat completion request for Anthropic API
type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	Messages      []AnthropicMessage `json:"messages"`
	System        string             `json:"system,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

// AnthropicResponse represents a chat response from Anthropic API
type AnthropicResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Role       string             `json:"role"`
	Content    []AnthropicContent `json:"content"`
	Model      string             `json:"model"`
	StopReason string             `json:"stop_reason"`
	Usage      AnthropicUsage     `json:"usage"`
}

// AnthropicMessage represents a message in Anthropic format
type AnthropicMessage struct {
	Role    string             `json:"role"`    // "user" or "assistant"
	Content []AnthropicContent `json:"content"`
}

// AnthropicContent represents content within a message
type AnthropicContent struct {
	Type   string       `json:"type"`   // "text" or "image"
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
}

// ImageSource represents an image source
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/jpeg", etc.
	Data      string `json:"data"`       // base64 data
}

// AnthropicUsage represents token usage information
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// PerfMetrics tracks performance metrics for LLM responses
type PerfMetrics struct {
	startTime       time.Time
	totalTokens     int
	tokenCount      int
	responseTime    time.Duration
	windowSize      int // Total context window size
	usedTokens      int // Total tokens used in context
	remainingTokens int // Remaining tokens in context window
}

func (p *PerfMetrics) start() {
	p.startTime = time.Now()
	p.totalTokens = 0
	p.tokenCount = 0
}

func (p *PerfMetrics) addTokens(text string) {
	// Simple token counting - splitting on spaces and punctuation
	p.totalTokens += len(strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	}))
	p.tokenCount++
}

func (p *PerfMetrics) updateContextStats(windowSize, usedTokens int) {
	p.windowSize = windowSize
	p.usedTokens = usedTokens
	p.remainingTokens = windowSize - usedTokens
	if p.remainingTokens < 0 {
		p.remainingTokens = 0
	}
}

func (p *PerfMetrics) finish() {
	p.responseTime = time.Since(p.startTime)
}

func (p *PerfMetrics) String() string {
	tps := float64(p.totalTokens) / p.responseTime.Seconds()
	var output strings.Builder

	output.WriteString("\n[Performance Metrics]")
	output.WriteString(fmt.Sprintf("\n- Response Time: %v", p.responseTime.Round(time.Millisecond)))
	output.WriteString(fmt.Sprintf("\n- Tokens/Second: %.2f", tps))
	output.WriteString(fmt.Sprintf("\n- Response Size: %d tokens", p.totalTokens))

	if p.windowSize > 0 {
		usagePercent := float64(p.usedTokens) / float64(p.windowSize) * 100
		output.WriteString("\n\n[Context Window]")
		output.WriteString(fmt.Sprintf("\n- Window Size:  %d tokens", p.windowSize))
		output.WriteString(fmt.Sprintf("\n- Used:         %d tokens (%.1f%%)", p.usedTokens, usagePercent))
		output.WriteString(fmt.Sprintf("\n- Remaining:    %d tokens", p.remainingTokens))

		if usagePercent > 90 {
			output.WriteString(fmt.Sprintf("\n\n⚠️  Warning: Using %.1f%% of context window", usagePercent))
		}
	}

	output.WriteString("\n")
	return output.String()
}

func (p *PerfMetrics) JSON() string {
	metrics := struct {
		TokensPerSecond float64 `json:"tokens_per_second"`
		TotalTokens     int     `json:"total_tokens"`
		ResponseTimeMs  int64   `json:"response_time_ms"`
		WindowSize      int     `json:"context_window_size,omitempty"`
		UsedTokens      int     `json:"used_tokens,omitempty"`
		RemainingTokens int     `json:"remaining_tokens,omitempty"`
		WindowUsagePerc float64 `json:"window_usage_percentage,omitempty"`
	}{
		TokensPerSecond: float64(p.totalTokens) / p.responseTime.Seconds(),
		TotalTokens:     p.totalTokens,
		ResponseTimeMs:  p.responseTime.Milliseconds(),
	}

	if p.windowSize > 0 {
		metrics.WindowSize = p.windowSize
		metrics.UsedTokens = p.usedTokens
		metrics.RemainingTokens = p.remainingTokens
		metrics.WindowUsagePerc = float64(p.usedTokens) / float64(p.windowSize) * 100
	}

	jsonBytes, err := json.Marshal(metrics)
	if err != nil {
		return fmt.Sprintf(`{"error": "failed to marshal metrics: %v"}`, err)
	}
	return string(jsonBytes)
}

// ContextFile represents a loaded file in the context
type ContextFile struct {
	Name     string
	Content  string
	Language string
}

// ContextStats tracks context window usage
type ContextStats struct {
	WindowSize      int     // Maximum context size (from model options or default)
	UsedTokens      int     // Estimated tokens used in context
	RemainingTokens int     // Estimated remaining tokens
	UsagePercent    float64 // Percentage of context window used
}

// estimateTokenCount provides an improved estimate of tokens for Claude models
func estimateTokenCount(text string) int {
	// Improved estimation for Claude models
	// Claude models generally use ~4 characters per token for English text
	// This is still an approximation - actual tokenization varies by content
	chars := len(text)
	
	// Account for different text types
	words := len(strings.Fields(text))
	if words == 0 {
		return chars / 4
	}
	
	avgWordLength := float64(chars) / float64(words)
	
	// Shorter words tend to be more tokens per character
	// Longer words tend to be fewer tokens per character
	if avgWordLength < 4 {
		return int(float64(chars) * 0.3) // ~3.3 chars per token
	} else if avgWordLength > 6 {
		return int(float64(chars) * 0.2) // ~5 chars per token
	}
	
	return chars / 4 // Default 4 chars per token
}

// getContextStats calculates context window usage including all messages
func (c *AnthropicClient) getContextStats() ContextStats {
	// Get context window size using our Claude-aware method
	windowSize := c.getContextWindow()

	// Calculate tokens from context files
	var contextTokens int
	for _, file := range c.context {
		contextTokens += estimateTokenCount(file.Content)
	}

	// Calculate tokens from history
	var historyTokens int
	if c.history != nil {
		historyTokens = c.history.EstimateTokenCount()
	}

	// Total tokens used is context + history
	totalTokens := contextTokens + historyTokens

	// Calculate remaining space and usage percentage
	remaining := windowSize - totalTokens
	if remaining < 0 {
		remaining = 0
	}
	usagePercent := float64(totalTokens) / float64(windowSize) * 100

	return ContextStats{
		WindowSize:      windowSize,
		UsedTokens:      totalTokens,
		RemainingTokens: remaining,
		UsagePercent:    usagePercent,
	}
}

// AnthropicClient handles communication with Anthropic API
type AnthropicClient struct {
	provider     string // "direct" or "bedrock"
	baseURL      string
	region       string // For Bedrock
	version      string // Anthropic API version
	httpClient   *http.Client
	context      []ContextFile
	model        *ModelDefinition
	defaultModel string

	history      *ConversationHistory
	showContext  bool      // Whether to show prompts and context before sending to LLM
	lastContext  []Message // Stores the last context sent to the LLM
	lastMetrics  *PerfMetrics
}

func (c *AnthropicClient) loadModel(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read model file: %v", err)
	}

	var model ModelDefinition
	if err := json.Unmarshal(data, &model); err != nil {
		return fmt.Errorf("failed to parse model file: %v", err)
	}

	// For Anthropic models, validate the model name format
	if model.Name == "" {
		return fmt.Errorf("model name is required")
	}

	// Validate provider
	if model.Provider == "" {
		model.Provider = "direct" // Default to direct API
	}
	if model.Provider != "direct" && model.Provider != "bedrock" {
		return fmt.Errorf("provider must be 'direct' or 'bedrock'")
	}

	// Store the model configuration
	c.model = &model
	return nil
}

// detectFileLanguage attempts to detect the programming language from the file extension
func detectFileLanguage(filename string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	switch ext {
	case "go":
		return "Go"
	case "js":
		return "JavaScript"
	case "ts":
		return "TypeScript"
	case "py":
		return "Python"
	case "java":
		return "Java"
	case "c":
		return "C"
	case "cpp", "cc":
		return "C++"
	case "rs":
		return "Rust"
	case "md":
		return "Markdown"
	default:
		return "plaintext"
	}
}

func NewAnthropicClient(provider, baseURL, region, defaultModel string) *AnthropicClient {
	if provider == "" {
		provider = "direct"
	}
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if region == "" {
		region = "us-east-1"
	}
	if defaultModel == "" {
		defaultModel = "claude-3-5-sonnet-20241022"
	}
	
	client := &AnthropicClient{
		provider:     provider,
		baseURL:      baseURL,
		region:       region,
		version:      "2023-06-01",
		httpClient:   &http.Client{},
		defaultModel: defaultModel,
	}
	
	return client
}

// initializeAuthentication validates authentication and provides helpful error messages
func (c *AnthropicClient) initializeAuthentication() error {
	if err := c.validateAuthentication(); err != nil {
		switch c.provider {
		case "direct":
			return fmt.Errorf("Authentication setup error: %v\n\nTo use Anthropic's direct API:\n1. Get an API key from https://console.anthropic.com/\n2. Set environment variable: export ANTHROPIC_API_KEY=\"sk-ant-your-key-here\"", err)
		case "bedrock":
			return fmt.Errorf("Authentication setup error: %v\n\nTo use Anthropic via Bedrock:\n1. Configure AWS credentials (aws configure or environment variables)\n2. Ensure you have access to Claude models in Bedrock", err)
		default:
			return err
		}
	}
	return nil
}

func (c *AnthropicClient) loadFile(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read file: %v", err)
	}

	// Get current context stats
	stats := c.getContextStats()
	newTokens := estimateTokenCount(string(content))

	// Check if adding this file would exceed the context window
	if stats.UsedTokens+newTokens > stats.WindowSize {
		return fmt.Errorf("adding this file (%d tokens) would exceed the context window size of %d tokens (currently using %d tokens)",
			newTokens, stats.WindowSize, stats.UsedTokens)
	}

	filename := filepath.Base(path)
	language := detectFileLanguage(filename)

	c.context = append(c.context, ContextFile{
		Name:     filename,
		Content:  string(content),
		Language: language,
	})

	return nil
}

func (c *AnthropicClient) createModelTemplate(path string) error {
	template := ModelDefinition{
		Name:     "claude-3-5-sonnet-20241022",
		Provider: "direct",
		Parameters: AnthropicParameters{
			MaxTokens:     4096,
			Temperature:   0.7,
			TopP:          0.9,
			TopK:          40,
			StopSequences: []string{},
		},
		System: "You are a helpful assistant with expertise in software development.",
		Format: "markdown",
	}

	data, err := json.MarshalIndent(template, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal template: %v", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write template file: %v", err)
	}

	return nil
}

func (c *AnthropicClient) buildContextMessage() string {
	if len(c.context) == 0 {
		return ""
	}

	var b strings.Builder

	// Get context usage stats
	stats := c.getContextStats()
	b.WriteString(fmt.Sprintf("Context Window Usage: %.1f%% (%d/%d tokens, %d remaining)\n\n",
		stats.UsagePercent, stats.UsedTokens, stats.WindowSize, stats.RemainingTokens))

	b.WriteString("Files in context:\n\n")

	for _, file := range c.context {
		tokens := estimateTokenCount(file.Content)
		b.WriteString(fmt.Sprintf("File: %s (Language: %s, ~%d tokens)\n", file.Name, file.Language, tokens))
		b.WriteString("```" + strings.ToLower(file.Language) + "\n")
		b.WriteString(file.Content)
		b.WriteString("\n```\n\n")
	}

	return b.String()
}

// convertHistoryToAnthropicFormat converts conversation history to Anthropic message format
func (c *AnthropicClient) convertHistoryToAnthropicFormat() []AnthropicMessage {
	var messages []AnthropicMessage
	
	for _, msg := range c.history.Messages {
		if msg.Role == "system" {
			continue // System messages handled separately in Anthropic API
		}
		
		anthMsg := AnthropicMessage{
			Role: msg.Role,
			Content: []AnthropicContent{{
				Type: "text",
				Text: msg.Content,
			}},
		}
		messages = append(messages, anthMsg)
	}
	
	return messages
}

// convertAnthropicToDisplayFormat converts Anthropic response content to display text
func convertAnthropicToDisplayFormat(content []AnthropicContent) string {
	var result strings.Builder
	
	for _, block := range content {
		if block.Type == "text" {
			result.WriteString(block.Text)
		}
	}
	
	return result.String()
}

// extractSystemPrompt extracts the system prompt from the current model or history
func (c *AnthropicClient) extractSystemPrompt() string {
	// First priority: model's system prompt
	if c.model != nil && c.model.System != "" {
		return c.model.System
	}
	
	// Second priority: system message in history
	if c.history != nil {
		for _, msg := range c.history.Messages {
			if msg.Role == "system" {
				return msg.Content
			}
		}
	}
	
	return ""
}

// ChatRequest is an alias for AnthropicRequest to maintain compatibility
type ChatRequest = AnthropicRequest

// ChatResponse is an alias for AnthropicResponse to maintain compatibility
type ChatResponse = AnthropicResponse

func (c *AnthropicClient) Chat(ctx context.Context, req *ChatRequest) error {
	metrics := &PerfMetrics{}
	metrics.start()

	// Convert history to Anthropic format
	anthropicMessages := c.convertHistoryToAnthropicFormat()
	systemPrompt := c.extractSystemPrompt()

	// Build proper Anthropic request
	anthropicReq := &AnthropicRequest{
		Model:     c.defaultModel,
		MaxTokens: 4096, // Default
		Messages:  anthropicMessages,
		System:    systemPrompt,
		Stream:    true,
	}

	// Override with model configuration if available
	if c.model != nil {
		anthropicReq.Model = c.model.Name
		if c.model.Parameters.MaxTokens > 0 {
			anthropicReq.MaxTokens = c.model.Parameters.MaxTokens
		}
		if c.model.Parameters.Temperature > 0 {
			anthropicReq.Temperature = &c.model.Parameters.Temperature
		}
		if c.model.Parameters.TopP > 0 {
			anthropicReq.TopP = &c.model.Parameters.TopP
		}
		if c.model.Parameters.TopK > 0 {
			anthropicReq.TopK = &c.model.Parameters.TopK
		}
		if len(c.model.Parameters.StopSequences) > 0 {
			anthropicReq.StopSequences = c.model.Parameters.StopSequences
		}
	}

	// Store context for reference
	c.lastContext = req.Messages

	// Get context window stats
	stats := c.getContextStats()
	metrics.updateContextStats(stats.WindowSize, stats.UsedTokens)

	// Show context if requested
	if c.showContext {
		fmt.Println("\nComplete request to be sent:")
		fmt.Println("============================")
		fmt.Printf("Model: %s\n", anthropicReq.Model)
		fmt.Printf("Provider: %s\n", c.provider)
		if anthropicReq.System != "" {
			fmt.Printf("System: %s\n", anthropicReq.System)
		}

		// Show parameters
		fmt.Println("\nActive Parameters:")
		fmt.Printf("  MaxTokens: %d\n", anthropicReq.MaxTokens)
		if anthropicReq.Temperature != nil {
			fmt.Printf("  Temperature: %.2f\n", *anthropicReq.Temperature)
		}
		if anthropicReq.TopP != nil {
			fmt.Printf("  TopP: %.2f\n", *anthropicReq.TopP)
		}
		if anthropicReq.TopK != nil {
			fmt.Printf("  TopK: %d\n", *anthropicReq.TopK)
		}

		// Show messages
		fmt.Printf("\nMessages (%d):\n", len(anthropicReq.Messages))
		for i, msg := range anthropicReq.Messages {
			content := convertAnthropicToDisplayFormat(msg.Content)
			if len(content) > 100 {
				content = content[:100] + "..."
			}
			fmt.Printf("%d. [%s]: %s\n", i+1, msg.Role, content)
		}

		fmt.Print("\nSend this request? [Y/n]: ")
		reader := bufio.NewReader(os.Stdin)
		response, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("failed to read user input: %v", err)
		}
		response = strings.TrimSpace(strings.ToLower(response))
		if response == "n" || response == "no" {
			return fmt.Errorf("submission cancelled by user")
		}
		fmt.Println()
	}

	// Marshal request
	jsonBody, err := json.Marshal(anthropicReq)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %v", err)
	}

	// Create HTTP request
	url := c.baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Add authentication headers
	if err := c.addAuthHeaders(httpReq); err != nil {
		return fmt.Errorf("failed to add auth headers: %v", err)
	}

	// Send request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var fullResponse strings.Builder

	if anthropicReq.Stream {
		// Handle streaming response (to be implemented in Phase 1.5)
		return fmt.Errorf("streaming responses will be implemented in Phase 1.5")
	} else {
		// Handle non-streaming response
		var anthropicResp AnthropicResponse
		if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
			return fmt.Errorf("failed to decode response: %v", err)
		}

		// Extract content from response
		content := convertAnthropicToDisplayFormat(anthropicResp.Content)
		fullResponse.WriteString(content)
		fmt.Print(content)
		metrics.addTokens(content)

		// Update metrics with usage info
		if anthropicResp.Usage.OutputTokens > 0 {
			metrics.totalTokens = anthropicResp.Usage.OutputTokens
		}
	}

	metrics.finish()
	fmt.Print(metrics)

	// Add response to conversation history
	if c.history != nil {
		c.history.AddAssistantMessage(fullResponse.String())
	}

	// Store metrics
	c.lastMetrics = metrics

	return nil
}

func setupMCP() {
	// TODO: Implement MCP setup once the correct MCP Go library is identified
	// The current code references non-existent packages:
	// - http.NewHTTPClientTransport (not a standard library function)
	// - mcp_golang (package not found)
	
	log.Println("MCP setup not implemented yet")
}

// showCommands prints the list of available commands
func showCommands() {
	fmt.Println("Available commands:")
	fmt.Println("  /help           - Show this help message")
	fmt.Println("  /load <file>    - Load a file into context")
	fmt.Println("  /model <file>   - Load model configuration from file")
	fmt.Println("  /status         - Show current model and context status")
	fmt.Println("  /history        - Show conversation history")
	fmt.Println("  /clear          - Clear conversation history")
	fmt.Println("  /dump           - Dump context to file")
	fmt.Println("  exit            - Exit the program")
	fmt.Println()
}

func main() {
		}

		// Calculate token estimates per message type
		var systemTokens, contextTokens, userTokens, assistantTokens int
		for _, msg := range req.Messages {
			tokens := estimateTokenCount(msg.Content)
			switch msg.Role {
			case "system":
				systemTokens += tokens
			case "user":
				// First user message after system is typically context
				if systemTokens > 0 && userTokens == 0 && contextTokens == 0 && strings.Contains(msg.Content, "Here is the current context") {
					contextTokens = tokens
				} else {
					userTokens += tokens
				}
			case "assistant":
				assistantTokens += tokens
			}
		}
		totalTokens := systemTokens + contextTokens + userTokens + assistantTokens

		// Calculate expected response size based on context
		// Rule of thumb: responses might be ~2x the size of the prompt
		expectedResponseTokens := userTokens * 2
		if numPredict < expectedResponseTokens {
			fmt.Printf("\n⚠️  Warning: NumPredict (%d) may be too small for expected response size (%d tokens)\n",
				numPredict, expectedResponseTokens)
			fmt.Println("   Consider increasing NumPredict in the model definition if responses are being truncated.")
		}

		// Show token usage breakdown
		fmt.Println("\nEstimated Token Usage:")
		fmt.Println("---------------------")
		if systemTokens > 0 {
			fmt.Printf("System Messages:  %7d tokens\n", systemTokens)
		}
		if contextTokens > 0 {
			fmt.Printf("Loaded Context:   %7d tokens\n", contextTokens)
		}
		fmt.Printf("User Messages:   %7d tokens\n", userTokens)
		fmt.Printf("AI Responses:    %7d tokens\n", assistantTokens)
		fmt.Printf("Total Size:      %7d tokens\n", totalTokens)

		// Show context window utilization
		contextWindow := 4096 // Default
		if c.model != nil && c.model.Options.NumCtx > 0 {
			contextWindow = c.model.Options.NumCtx
		}
		usagePercent := float64(totalTokens) / float64(contextWindow) * 100
		fmt.Printf("Context Window:  %7d tokens\n", contextWindow)
		fmt.Printf("Window Usage:    %7.1f%%\n", usagePercent)

		if usagePercent > 90 {
			fmt.Printf("\n⚠️  Warning: Request is using %.1f%% of the context window!\n", usagePercent)
		}

		fmt.Println("\nConversation Context and Messages:")
		fmt.Println("--------------------------------")
		caser := cases.Title(language.English)
		for i, msg := range req.Messages {
			role := caser.String(msg.Role)
			if i > 0 {
				fmt.Println() // Add blank line between messages
			}
			fmt.Printf("[%s]:\n%s", role, msg.Content)
		}

		fmt.Printf("\n\nReady to submit? (yes/no): ")

		var response string
		fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))

		if response != "yes" && response != "y" {
			return fmt.Errorf("submission cancelled by user")
		}
		fmt.Println("\nSubmitting request...")
	}

	// Prepare request
	if c.model != nil {
		req.Model = c.model.Name

		// Map model parameters to Anthropic parameters
		params := c.model.Parameters
		if params.Temperature > 0 {
			req.Temperature = &params.Temperature
		}
		if params.TopP > 0 {
			req.TopP = &params.TopP
		}
		if params.NumPredict > 0 {
			req.MaxTokens = &params.NumPredict
		}
		if params.RepeatPenalty > 0 {
			// Convert repeat_penalty to frequency_penalty (different scale)
			freqPenalty := (params.RepeatPenalty - 1.0) * 0.5
			if freqPenalty > 2.0 {
				freqPenalty = 2.0
			}
			req.FrequencyPenalty = &freqPenalty
		}
		if params.Seed > 0 {
			req.Seed = &params.Seed
		}
		if params.Stop != "" {
			req.Stop = []string{params.Stop}
		}
	} else {
		req.Model = c.defaultModel
	}

	jsonBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %v", err)
	}

	url := c.baseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Add Anthropic authentication headers
	if err := c.addAuthHeaders(httpReq); err != nil {
		return fmt.Errorf("failed to add auth headers: %v", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var fullResponse strings.Builder
	if req.Stream {
		// Handle streaming response
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				if data == "[DONE]" {
					break
				}

				var chatResp ChatResponse
				if err := json.Unmarshal([]byte(data), &chatResp); err != nil {
					continue // Skip malformed responses
				}

				if len(chatResp.Choices) > 0 && chatResp.Choices[0].Delta != nil {
					content := chatResp.Choices[0].Delta.Content
					if content != "" {
						// Only print the user's prompt in regular mode if showContext is false
						if metrics.totalTokens == 0 && !c.showContext {
							for _, msg := range req.Messages {
								if msg.Role == "user" {
									fmt.Printf("\nPrompt: %s\n\n", msg.Content)
									break
								}
							}
						}

						// Accumulate and print response
						fullResponse.WriteString(content)
						fmt.Print(content)
						metrics.addTokens(content)
					}
				}
			}
		}
	} else {
		// Handle non-streaming response
		var chatResp ChatResponse
		if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
			return fmt.Errorf("failed to decode response: %v", err)
		}

		if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message != nil {
			content := chatResp.Choices[0].Message.Content
			fullResponse.WriteString(content)
			fmt.Print(content)
			metrics.addTokens(content)
		}
	}

	metrics.finish()
	fmt.Print(metrics)

	// Add response to conversation history
	if c.history != nil {
		c.history.AddAssistantMessage(fullResponse.String())
	}

	// Save the last context for potential dumping later
	c.lastContext = req.Messages

	return nil
}

func setupMCP() {
	// TODO: Implement MCP setup once the correct MCP Go library is identified
	// The current code references non-existent packages:
	// - http.NewHTTPClientTransport (not a standard library function)
	// - mcp_golang (package not found)
	
	log.Println("MCP setup not implemented yet")
}

// showCommands prints the list of available commands
func showCommands() {
	fmt.Println("Available commands:")
	fmt.Println("  /help           - Show this help message")
	fmt.Println("  /load <file>    - Load a file into the context")
	fmt.Println("  /model <file>   - Load a model definition file")
	fmt.Println("  /status         - Show current model and context status")
	fmt.Println("  /history        - Show conversation history")
	fmt.Println("  /clear          - Clear conversation history")
	fmt.Println("  /dump           - Write current context to context-dump.txt")
	fmt.Println("  exit            - Exit the program")
	fmt.Println()
}

func main() {
	var flags struct {
		provider     string
		baseURL      string
		region       string
		prompt       string
		modelConfig  string
		defaultModel string
		showContext  bool
	}

	// Parse command line flags
	flag.StringVar(&flags.provider, "provider", "direct", "Provider to use: direct or bedrock")
	flag.StringVar(&flags.baseURL, "url", "https://api.anthropic.com", "Base URL of the Anthropic API server")
	flag.StringVar(&flags.region, "region", "us-east-1", "AWS region for Bedrock")
	flag.StringVar(&flags.prompt, "prompt", "", "Path to initial prompt file")
	flag.StringVar(&flags.modelConfig, "model", "", "Path to model configuration file")
	flag.StringVar(&flags.defaultModel, "default-model", "claude-3-5-sonnet-20241022", "Default model to use if no model config is provided")
	flag.BoolVar(&flags.showContext, "context", false, "Show prompts and context before sending to LLM")
	flag.BoolVar(&flags.showContext, "c", false, "Show prompts and context before sending to LLM (shorthand)")
	flag.Parse()

	// Create Anthropic client
	anthropicClient := NewAnthropicClient(flags.provider, flags.baseURL, flags.region, flags.defaultModel)
	anthropicClient.history = NewConversationHistory("")
	anthropicClient.showContext = flags.showContext
	
	// Validate authentication early
	if err := anthropicClient.initializeAuthentication(); err != nil {
		log.Fatal(err)
	}

	// Try to load model if specified
	if flags.modelConfig != "" {
		if err := anthropicClient.loadModel(flags.modelConfig); err != nil {
			log.Printf("Failed to load model config: %v", err)
		} else {
			fmt.Printf("\nLoaded model configuration: %s", anthropicClient.model.Name)
			if anthropicClient.model.System != "" {
				fmt.Printf("System prompt: %s\n", anthropicClient.model.System)
				// Set system prompt in history if present
				anthropicClient.history = NewConversationHistory(anthropicClient.model.System)
			}
		}
	} else {
		fmt.Println("\nNo model definition loaded, using default model")
	}

	// Read prompt content if specified
	var promptContent []byte
	if flags.prompt != "" {
		var err error
		promptContent, err = os.ReadFile(flags.prompt)
		if err != nil {
			log.Fatalf("Failed to read prompt file: %v", err)
		}
		fmt.Printf("\nPrompt from %s:\n%s\n", flags.prompt, string(promptContent))
	}

	// Handle initial prompt if specified
	if flags.prompt != "" {
		promptStr := string(promptContent)
		anthropicClient.history.AddUserMessage(promptStr)

		// Prepare chat request with history
		req := &ChatRequest{
			Messages: anthropicClient.history.Messages,
			Stream:   true,
		}

		fmt.Printf("\nReading prompt from: %s\n", flags.prompt)
		if err := anthropicClient.Chat(context.Background(), req); err != nil {
			log.Printf("Error processing initial prompt: %v", err)
		}
		fmt.Println()
	}

	// Set up command history
	historyFile := filepath.Join(os.TempDir(), ".gchai_history")
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            "> ",
		HistoryFile:       historyFile,
		HistoryLimit:      64,
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true, // Case-insensitive history search
	})
	if err != nil {
		log.Fatal(err)
	}
	defer rl.Close()

	fmt.Println("Setting up connection to MCP server...")
	setupMCP()

	// Interactive prompt loop
	fmt.Println("Interactive AI Assistant")
	showCommands()
	fmt.Println() // Single blank line before starting input

	for {
		question, err := rl.Readline()
		if err != nil {
			if err == readline.ErrInterrupt {
				continue // Allow Ctrl-C to cancel current input
			} else if err == io.EOF {
				fmt.Println("\nGoodbye!") // Add newline for clean exit on Ctrl-D
				break
			}
			log.Printf("Error reading input: %v", err)
			continue
		}

		question = strings.TrimSpace(question)
		if question == "" {
			continue
		}

		if question == "exit" {
			fmt.Println("Goodbye!")
			break
		}

		// Handle help command
		if question == "/help" {
			showCommands()
			continue
		}

		// Handle file loading command
		if strings.HasPrefix(question, "/load ") {
			filePath := strings.TrimSpace(strings.TrimPrefix(question, "/load "))
			if err := anthropicClient.loadFile(filePath); err != nil {
				fmt.Printf("Error loading file: %v\n", err)
			} else {
				fmt.Printf("Loaded file: %s\n", filepath.Base(filePath))
			}
			continue
		}

		// Handle model loading command
		if strings.HasPrefix(question, "/model ") {
			filePath := strings.TrimSpace(strings.TrimPrefix(question, "/model "))
			if err := anthropicClient.loadModel(filePath); err != nil {
				fmt.Printf("Error loading model: %v\n", err)
			} else {
				fmt.Printf("Loaded and created model: %s\n", anthropicClient.model.Name)
				// Update system prompt in history if present
				if anthropicClient.model.System != "" {
					anthropicClient.history = NewConversationHistory(anthropicClient.model.System)
					fmt.Printf("System prompt: %s\n", anthropicClient.model.System)
				}
			}
			continue
		}

		// Clear history command
		if question == "/clear" {
			systemPrompt := ""
			if anthropicClient.model != nil {
				systemPrompt = anthropicClient.model.System
			}
			anthropicClient.history = NewConversationHistory(systemPrompt)
			fmt.Println("Conversation history cleared.")
			continue
		}

		// Show history command
		if question == "/history" {
			fmt.Println("\nConversation History:")
			caser := cases.Title(language.English)
			for _, msg := range anthropicClient.history.Messages {
				role := caser.String(msg.Role)
				fmt.Printf("%s: %s\n", role, msg.Content)
			}
			fmt.Printf("\nEstimated tokens: %d\n", anthropicClient.history.EstimateTokenCount())
			continue
		}

		// Show status command
		if question == "/status" {
			anthropicClient.showStatus()
			continue
		}

		// Handle dump command
		if question == "/dump" {
			if err := anthropicClient.dumpContextToFile("context-dump.txt"); err != nil {
				fmt.Printf("Error: %v\n", err)
			} else {
				fmt.Println("Context dumped to context-dump.txt")
			}
			continue
		}

		// Prepare messages array: context (if any) followed by conversation history
		messages := make([]Message, 0)
		if contextMsg := anthropicClient.buildContextMessage(); contextMsg != "" {
			messages = append(messages, Message{
				Role:    "user",
				Content: "Here is the current context. Use this information to answer my next question:\n\n" + contextMsg,
			})
		}

		// Add user question to history
		anthropicClient.history.AddUserMessage(question)

		// Trim history to fit context window if needed
		anthropicClient.history.TrimToFit(anthropicClient.getContextWindow())

		// Prepare messages array: context (if any) followed by conversation history
		if len(messages) > 0 {
			// If we have context, add it before the conversation history
			messages = append(messages, anthropicClient.history.Messages...)
		} else {
			messages = anthropicClient.history.Messages
		}

		// Prepare chat request
		req := &ChatRequest{
			Messages: messages,
			Stream:   true,
		}

		fmt.Println()
		if err := anthropicClient.Chat(context.Background(), req); err != nil {
			if err.Error() == "submission cancelled by user" {
				fmt.Println("Request cancelled. Type your next prompt or command.")
				continue
			}
			log.Printf("Error: %v", err)
		}
		fmt.Println()
	}
}

// ConversationHistory tracks the conversation between user and assistant
type ConversationHistory struct {
	Messages []Message
}

func NewConversationHistory(systemPrompt string) *ConversationHistory {
	history := &ConversationHistory{
		Messages: make([]Message, 0),
	}
	if systemPrompt != "" {
		history.Messages = append(history.Messages, Message{
			Role:    "system",
			Content: systemPrompt,
		})
	}
	return history
}

func (h *ConversationHistory) AddUserMessage(content string) {
	h.Messages = append(h.Messages, Message{
		Role:    "user",
		Content: content,
	})
}

func (h *ConversationHistory) AddAssistantMessage(content string) {
	h.Messages = append(h.Messages, Message{
		Role:    "assistant",
		Content: content,
	})
}

// EstimateTokenCount estimates the total tokens in the conversation history
func (h *ConversationHistory) EstimateTokenCount() int {
	var total int
	for _, msg := range h.Messages {
		total += estimateTokenCount(msg.Content)
	}
	return total
}

// TrimToFit ensures the conversation history fits within the given token limit
// by removing older messages while preserving the system message if present
func (h *ConversationHistory) TrimToFit(tokenLimit int) {
	// Return early if we're already under the limit
	if h.EstimateTokenCount() <= tokenLimit {
		return
	}

	// If we have a system message, temporarily remove it
	var systemMsg *Message
	if len(h.Messages) > 0 && h.Messages[0].Role == "system" {
		systemMsg = &h.Messages[0]
		h.Messages = h.Messages[1:]
	}

	// Remove messages from the start (oldest) until we're under the limit
	for len(h.Messages) > 2 && h.EstimateTokenCount() > tokenLimit {
		// Remove the oldest message pair (user + assistant)
		h.Messages = h.Messages[2:]
	}

	// Restore system message if we had one
	if systemMsg != nil {
		h.Messages = append([]Message{*systemMsg}, h.Messages...)
	}
}

// getContextWindow returns the model's context window size for Claude models
func (c *AnthropicClient) getContextWindow() int {
	// All Claude models have 200K context window
	return 200000
}

// showStatus prints the current model and context status
func (c *AnthropicClient) showStatus() {
	fmt.Println("\nCurrent Status:")
	fmt.Println("-------------")

	// Model information
	if c.model != nil {
		fmt.Printf("Model: %s\n", c.model.Name)
		if c.model.System != "" {
			fmt.Printf("System Prompt: %s\n", c.model.System)
		}

		// Show model parameters if any are set
		paramValue := reflect.ValueOf(c.model.Parameters)
		paramType := reflect.TypeOf(c.model.Parameters)
		fmt.Println("\nParameters:")
		for i := 0; i < paramType.NumField(); i++ {
			field := paramType.Field(i)
			value := paramValue.Field(i)
			if !value.IsZero() {
				fmt.Printf("  %s: %v\n", field.Name, value.Interface())
			}
		}

		// Show model options if any are set
		optValue := reflect.ValueOf(c.model.Options)
		optType := reflect.TypeOf(c.model.Options)
		hasOptions := false
		for i := 0; i < optType.NumField(); i++ {
			if !optValue.Field(i).IsZero() {
				if !hasOptions {
					fmt.Println("\nOptions:")
					hasOptions = true
				}
				field := optType.Field(i)
				value := optValue.Field(i)
				fmt.Printf("  %s: %v\n", field.Name, value.Interface())
			}
		}
	} else {
		fmt.Printf("Model: %s (default)\n", c.defaultModel)
	}

	// Detailed token usage
	fmt.Println("\nToken Usage:")
	fmt.Println("-----------")

	// Calculate system prompt tokens
	var systemTokens int
	if c.model != nil && c.model.System != "" {
		systemTokens = estimateTokenCount(c.model.System)
		fmt.Printf("System Prompt:    %7d tokens\n", systemTokens)
	}

	// Calculate context file tokens
	var contextTokens int
	if len(c.context) > 0 {
		for _, file := range c.context {
			contextTokens += estimateTokenCount(file.Content)
		}
		fmt.Printf("Context Files:    %7d tokens\n", contextTokens)
	}

	// Calculate conversation history tokens
	var historyStats struct {
		user      int
		assistant int
		total     int
	}

	if c.history != nil {
		for _, msg := range c.history.Messages {
			tokens := estimateTokenCount(msg.Content)
			switch msg.Role {
			case "system":
				// Already counted above
			case "user":
				historyStats.user += tokens
			case "assistant":
				historyStats.assistant += tokens
			}
			historyStats.total += tokens
		}
		if historyStats.user > 0 || historyStats.assistant > 0 {
			fmt.Printf("User Messages:    %7d tokens\n", historyStats.user)
			fmt.Printf("AI Responses:     %7d tokens\n", historyStats.assistant)
		}
	}

	// Total and remaining tokens
	totalTokens := systemTokens + contextTokens + historyStats.total
	windowSize := c.getContextWindow()
	remaining := windowSize - totalTokens
	if remaining < 0 {
		remaining = 0
	}
	usagePercent := float64(totalTokens) / float64(windowSize) * 100

	fmt.Printf("\nTotal Used:       %7d tokens\n", totalTokens)
	fmt.Printf("Window Size:      %7d tokens\n", windowSize)
	fmt.Printf("Remaining:        %7d tokens\n", remaining)
	fmt.Printf("Window Usage:     %7.1f%%\n", usagePercent)

	if usagePercent > 90 {
		fmt.Printf("\n⚠️  Warning: Currently using %.1f%% of the context window!\n", usagePercent)
	}

	// Context file information
	if len(c.context) > 0 {
		fmt.Println("\nLoaded Context Files:")
		for _, file := range c.context {
			tokens := estimateTokenCount(file.Content)
			fmt.Printf("  - %s (%s): %d tokens\n", file.Name, file.Language, tokens)
		}
	} else {
		fmt.Println("\nNo context files loaded")
	}

	// Conversation history status
	if c.history != nil {
		messageCount := len(c.history.Messages)
		if messageCount > 0 {
			systemCount := 0
			if messageCount > 0 && c.history.Messages[0].Role == "system" {
				systemCount = 1
			}
			userCount := (messageCount - systemCount) / 2
			fmt.Printf("\nConversation History: %d exchanges", userCount)
			if systemCount > 0 {
				fmt.Print(" (with system prompt)")
			}
			fmt.Printf("\nHistory Size: %d tokens\n", historyStats.total)
		} else {
			fmt.Println("\nNo conversation history")
		}
	}
	fmt.Println()
}

// dumpContextToFile writes the last sent context to a file
func (c *AnthropicClient) dumpContextToFile(filename string) error {
	if len(c.lastContext) == 0 {
		return fmt.Errorf("no context available - send a prompt first")
	}

	var output strings.Builder
	output.WriteString("Last Context Sent to LLM\n")
	output.WriteString("======================\n\n")

	// Add model information
	if c.model != nil {
		output.WriteString(fmt.Sprintf("Model: %s\n", c.model.Name))
		if c.model.System != "" {
			output.WriteString(fmt.Sprintf("System Prompt: %s\n", c.model.System))
		}

		// Show active parameters
		paramValue := reflect.ValueOf(c.model.Parameters)
		paramType := reflect.TypeOf(c.model.Parameters)
		hasParams := false
		for i := 0; i < paramType.NumField(); i++ {
			field := paramType.Field(i)
			value := paramValue.Field(i)
			if !value.IsZero() {
				if !hasParams {
					output.WriteString("\nActive Parameters:\n")
					hasParams = true
				}
				output.WriteString(fmt.Sprintf("  %s: %v\n", field.Name, value.Interface()))
			}
		}
	} else {
		output.WriteString(fmt.Sprintf("Model: %s (default)\n", c.defaultModel))
	}

	// Calculate token estimates per message type
	var systemTokens, contextTokens, userTokens, assistantTokens int
	caser := cases.Title(language.English)

	output.WriteString("\nMessages:\n")
	output.WriteString("---------\n")
	for i, msg := range c.lastContext {
		tokens := estimateTokenCount(msg.Content)
		role := caser.String(msg.Role)

		// Add blank line between messages for readability
		if i > 0 {
			output.WriteString("\n")
		}

		output.WriteString(fmt.Sprintf("[%s] (%d tokens):\n%s\n", role, tokens, msg.Content))

		switch msg.Role {
		case "system":
			systemTokens += tokens
		case "user":
			if systemTokens > 0 && userTokens == 0 && contextTokens == 0 && strings.Contains(msg.Content, "Here is the current context") {
				contextTokens = tokens
			} else {
				userTokens += tokens
			}
		case "assistant":
			assistantTokens += tokens
		}
	}

	// Add token usage summary
	totalTokens := systemTokens + contextTokens + userTokens + assistantTokens
	output.WriteString("\nToken Usage Summary:\n")
	output.WriteString("-----------------\n")
	if systemTokens > 0 {
		output.WriteString(fmt.Sprintf("System Messages:  %7d tokens\n", systemTokens))
	}
	if contextTokens > 0 {
		output.WriteString(fmt.Sprintf("Loaded Context:   %7d tokens\n", contextTokens))
	}
	output.WriteString(fmt.Sprintf("User Messages:   %7d tokens\n", userTokens))
	output.WriteString(fmt.Sprintf("AI Responses:    %7d tokens\n", assistantTokens))
	output.WriteString(fmt.Sprintf("Total Size:      %7d tokens\n", totalTokens))

	// Get context window info
	windowSize := 4096 // Default
	if c.model != nil && c.model.Options.NumCtx > 0 {
		windowSize = c.model.Options.NumCtx
	}
	usagePercent := float64(totalTokens) / float64(windowSize) * 100
	output.WriteString(fmt.Sprintf("Context Window:  %7d tokens\n", windowSize))
	output.WriteString(fmt.Sprintf("Window Usage:    %7.1f%%\n", usagePercent))

	// Write to file
	err := os.WriteFile(filename, []byte(output.String()), 0644)
	if err != nil {
		return fmt.Errorf("failed to write context dump: %v", err)
	}

	return nil
}

// addAuthHeaders adds Anthropic authentication headers to the request
func (c *AnthropicClient) addAuthHeaders(req *http.Request) error {
	switch c.provider {
	case "direct":
		return c.addDirectAnthropicAuth(req)
	case "bedrock":
		return c.addBedrockAuth(req)
	default:
		return fmt.Errorf("unsupported provider: %s", c.provider)
	}
}

// addDirectAnthropicAuth adds authentication headers for direct Anthropic API
func (c *AnthropicClient) addDirectAnthropicAuth(req *http.Request) error {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY environment variable is required for direct API access")
	}
	
	// Validate API key format
	if !strings.HasPrefix(apiKey, "sk-ant-") {
		return fmt.Errorf("invalid ANTHROPIC_API_KEY format (should start with 'sk-ant-')")
	}
	
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", c.version)
	req.Header.Set("Content-Type", "application/json")
	
	return nil
}

// addBedrockAuth adds authentication headers for Bedrock (placeholder for Phase 3)
func (c *AnthropicClient) addBedrockAuth(req *http.Request) error {
	// Bedrock authentication will be implemented in Phase 3
	// For now, return an error indicating it's not yet implemented
	return fmt.Errorf("Bedrock authentication not yet implemented - will be added in Phase 3")
}

// validateAuthentication checks if the current authentication setup is valid
func (c *AnthropicClient) validateAuthentication() error {
	switch c.provider {
	case "direct":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return fmt.Errorf("ANTHROPIC_API_KEY environment variable is required for direct API access")
		}
		if !strings.HasPrefix(apiKey, "sk-ant-") {
			return fmt.Errorf("invalid ANTHROPIC_API_KEY format (should start with 'sk-ant-')")
		}
		return nil
	case "bedrock":
		// Basic AWS credentials check - full implementation in Phase 3
		if os.Getenv("AWS_ACCESS_KEY_ID") == "" && os.Getenv("AWS_PROFILE") == "" {
			return fmt.Errorf("AWS credentials required for Bedrock (set AWS_ACCESS_KEY_ID or AWS_PROFILE)")
		}
		return nil
	default:
		return fmt.Errorf("unsupported provider: %s", c.provider)
	}
}
