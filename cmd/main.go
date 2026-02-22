package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/telebot.v3"
)

var (
	logger      = slog.Default().With(slog.String("package", "main"))
	httpClient = &http.Client{}
	bot        *telebot.Bot
	mu         sync.Mutex
	userQueues = make(map[int64]chan string) // Message queue per user
)

// isAllowed checks if the user is in the allowed list
func isAllowed(userID int64) bool {
	allowedIface := viper.Get("allowed_users")
	if allowedIface == nil {
		return true // Allow all if no list configured
	}
	allowed, ok := allowedIface.([]interface{})
	if !ok || len(allowed) == 0 {
		return true // Allow all if no list configured
	}
	for _, item := range allowed {
		// Handle both int and float (JSON numbers)
		switch v := item.(type) {
		case int64:
			if v == userID {
				return true
			}
		case int:
			if int64(v) == userID {
				return true
			}
		case float64:
			if int64(v) == userID {
				return true
			}
		}
	}
	return false
}

// Config
type Config struct {
	APIToken     string   `mapstructure:"api_token"`     // Telegram bot token
	APIEndpoint  string   `mapstructure:"api_endpoint"`  // OpenAI-compatible endpoint
	APIKey       string   `mapstructure:"api_key"`      // API key for the LLM
	DefaultModel string   `mapstructure:"default_model"` // Default model
	AllowedUsers []int64  `mapstructure:"allowed_users"` // Allowed Telegram user IDs
	MaxTokens    int      `mapstructure:"max_tokens"`    // Max tokens for LLM response (default 16000)
	TimeoutSecs  int      `mapstructure:"timeout_secs"`  // API timeout in seconds (default 300)
}

// User state
type UserState struct {
	Model        string                   `json:"model"`
	SystemPrompt string                   `json:"system_prompt"`
	History      []ChatMessage            `json:"history"`
	Presets      map[string]Preset       `json:"presets"`
	PendingInput string                   `json:"pending_input"` // "model" or "system" if waiting for input
}

type Preset struct {
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
}

var userStates = make(map[int64]*UserState)

// Load user state from disk
func loadUserState(chatID int64) *UserState {
	state := &UserState{
		Model:        viper.GetString("default_model"),
		SystemPrompt: "You are a helpful assistant.",
		Presets:      make(map[string]Preset),
	}

	filePath := getStateFilePath(chatID)
	data, err := os.ReadFile(filePath)
	if err != nil {
		// Try to load preset 1 by default
		state.Presets["1"] = Preset{Model: state.Model, SystemPrompt: state.SystemPrompt}
		return state
	}

	json.Unmarshal(data, state)
	
	// If no presets, set current as preset 1
	if len(state.Presets) == 0 {
		state.Presets["1"] = Preset{Model: state.Model, SystemPrompt: state.SystemPrompt}
	}
	
	return state
}

// Save user state to disk
func saveUserState(chatID int64, state *UserState) {
	data, _ := json.Marshal(state)
	os.WriteFile(getStateFilePath(chatID), data, 0644)
}

func getStateFilePath(chatID int64) string {
	return "./data/store/user_" + int64ToString(chatID) + ".json"
}

func int64ToString(i int64) string {
	return fmt.Sprintf("%d", i)
}

// API types
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type ChatResponse struct {
	Choices []Choice `json:"choices"`
}

type Choice struct {
	Message Message `json:"message"`
}

type Message struct {
	Content string `json:"content"`
}

// Fetch available models from API
func fetchModels() ([]string, error) {
	req, err := http.NewRequest("GET", viper.GetString("api_endpoint")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", "Bearer "+viper.GetString("api_key"))

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	// Handle different API formats
	if data, ok := result["data"].([]interface{}); ok {
		models := make([]string, 0, len(data))
		for _, m := range data {
			if mMap, ok := m.(map[string]interface{}); ok {
				if id, ok := mMap["id"].(string); ok {
					models = append(models, id)
				}
			}
		}
		return models, nil
	}

	// Fallback: try "models" key
	if models, ok := result["models"].([]interface{}); ok {
		result := make([]string, 0, len(models))
		for _, m := range models {
			if mMap, ok := m.(map[string]interface{}); ok {
				if id, ok := mMap["id"].(string); ok {
					result = append(result, id)
				}
			}
		}
		return result, nil
	}

	return nil, nil
}

// Send chat request
func sendChat(chatID int64, message string) (string, error) {
	state := userStates[chatID]
	if state == nil {
		state = loadUserState(chatID)
		userStates[chatID] = state
	}

	// Build messages: system + history + new message
	messages := []ChatMessage{}
	
	// Add system prompt
	if state.SystemPrompt != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: state.SystemPrompt})
	}
	
	// Add conversation history
	messages = append(messages, state.History...)
	
	// Add new user message
	messages = append(messages, ChatMessage{Role: "user", Content: message})

	maxTokens := viper.GetInt("max_tokens")
	if maxTokens <= 0 {
		maxTokens = 16000
	}

	reqBody := ChatRequest{
		Model:    state.Model,
		Messages: messages,
		Stream:   false,
		MaxTokens: maxTokens,
	}

	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequest("POST", viper.GetString("api_endpoint")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+viper.GetString("api_key"))

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Check HTTP status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Error("API request failed", slog.Int("status", resp.StatusCode))
		return "", fmt.Errorf("API request failed with status %d", resp.StatusCode)
	}

	// Parse response
	var response ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		logger.Error("failed to parse response", slog.Any("error", err))
		return "", err
	}

	if len(response.Choices) == 0 {
		return "", nil
	}

	assistantReply := response.Choices[0].Message.Content

	// Add to conversation history
	state.History = append(state.History, ChatMessage{Role: "user", Content: message})
	state.History = append(state.History, ChatMessage{Role: "assistant", Content: assistantReply})
	
	// Keep history manageable (last 20 messages = 10 exchanges)
	if len(state.History) > 40 {
		state.History = state.History[len(state.History)-40:]
	}

	// Save state
	saveUserState(chatID, state)

	return assistantReply, nil
}

// processMessageQueue handles queued messages for a user one at a time
func processMessageQueue(chatID int64, c telebot.Context) {
	queue := userQueues[chatID]
	
	for msg := range queue {
		// Show typing indicator
		bot.Notify(c.Chat(), telebot.Typing)
		
		response, err := sendChat(chatID, msg)
		if err != nil {
			errMsg := err.Error()
			if strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "deadline") {
				c.Send("Request timed out. Try a shorter prompt or increase timeout_secs in config.")
			} else {
				c.Send("Error: " + errMsg)
			}
			continue
		}
		
		if response == "" {
			c.Send("No response received.")
			continue
		}
		
		logger.Info("response received", slog.Int("length", len(response)), slog.Int("tokens_approx", len(response)/4))
		
		// Try plain text first
		err = c.Send(response)
		if err != nil {
			logger.Warn("plain send failed, trying HTML", slog.Any("error", err))
			htmlResponse := convertMarkdownToHTML(response)
			err = c.Send(htmlResponse, telebot.ModeHTML)
			if err != nil {
				logger.Error("HTML send failed, splitting", slog.Any("error", err))
				splitAndSend(c, response)
			}
		}
	}
	
	// Clean up when queue is closed
	mu.Lock()
	delete(userQueues, chatID)
	mu.Unlock()
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	logger = slog.Default().With(slog.String("package", "main"))

	// Load config
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("config")
	viper.AddConfigPath("data/config")
	viper.ReadInConfig()

	// Validate required config
	if viper.GetString("api_token") == "" {
		logger.Error("api_token is required in config")
		os.Exit(1)
	}
	if viper.GetString("api_endpoint") == "" {
		logger.Error("api_endpoint is required in config")
		os.Exit(1)
	}
	if viper.GetString("api_key") == "" {
		logger.Error("api_key is required in config")
		os.Exit(1)
	}
	if viper.GetString("default_model") == "" {
		logger.Error("default_model is required in config")
		os.Exit(1)
	}

	// Configure HTTP client with timeout
	timeoutSecs := viper.GetInt("timeout_secs")
	if timeoutSecs <= 0 {
		timeoutSecs = 300 // Default 5 minutes
	}
	httpClient.Timeout = time.Duration(timeoutSecs) * time.Second
	logger.Info("http client configured", slog.Int("timeout_secs", timeoutSecs))

	// Set default max tokens
	maxTokens := viper.GetInt("max_tokens")
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	logger.Info("max tokens configured", slog.Int("max_tokens", maxTokens))

	// Ensure data directory exists
	os.MkdirAll("./data/store", 0755)

	// Initialize bot
	logger.Info("creating bot with token", slog.String("token_prefix", viper.GetString("api_token")[:20]))
	b, err := telebot.NewBot(telebot.Settings{
		Token:  viper.GetString("api_token"),
		Poller: &telebot.LongPoller{},
	})
	if err != nil {
		logger.Error("failed to create bot", slog.Any("error", err))
		return
	}
	bot = b
	logger.Info("bot created successfully", slog.String("bot_name", b.Me.Username))

	// Start periodic cleanup of in-memory states (every 10 minutes)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			mu.Lock()
			for chatID := range userStates {
				// Keep only current user in memory, reload others from disk on next use
				if chatID != b.Me.ID {
					delete(userStates, chatID)
				}
			}
			mu.Unlock()
		}
	}()

	// Middleware to check allowed users
	b.Use(func(next telebot.HandlerFunc) telebot.HandlerFunc {
		return func(c telebot.Context) error {
			if !isAllowed(c.Sender().ID) {
				logger.Warn("unauthorized user tried to access bot", slog.Int64("user_id", c.Sender().ID))
				return c.Send("Sorry, this bot is not available to you.")
			}
			return next(c)
		}
	})

	// Commands
	b.Handle("/start", func(c telebot.Context) error {
		state := loadUserState(c.Chat().ID)
		userStates[c.Chat().ID] = state
		return c.Send("Welcome! I'm your AI assistant.\n\nCurrent model: "+state.Model+"\n\nCommands:\n/model - Switch model\n/models - List models\n/set <n> <model> <prompt> - Save preset\n/preset - List presets\n/preset <n> - Load preset\n/new - New conversation\n/reset - Reset system prompt")
	})

	b.Handle("/status", func(c telebot.Context) error {
		state := loadUserState(c.Chat().ID)
		userStates[c.Chat().ID] = state
		msg := "*Current Status*\n\n"
		msg += "Model: "+state.Model+"\n"
		msg += "System: "+state.SystemPrompt+"\n"
		msg += "History: " + fmt.Sprintf("%d", len(state.History)) + " messages"
		return c.Send(msg, telebot.ModeMarkdown)
	})

	b.Handle("/model", func(c telebot.Context) error {
		state := loadUserState(c.Chat().ID)
		userStates[c.Chat().ID] = state
		state.PendingInput = "model"
		saveUserState(c.Chat().ID, state)
		return c.Send("Send me the model name you want to use. Use /models to see available options.")
	})

	b.Handle("/models", func(c telebot.Context) error {
		c.Send("Fetching models...")
		models, err := fetchModels()
		if err != nil {
			return c.Send("Failed to fetch models: " + err.Error())
		}
		if models == nil {
			return c.Send("Could not parse models from API")
		}
		
		// Show first 20 models
		display := "Available models:\n\n"
		for i, m := range models {
			if i >= 20 {
				display += "\n...and " + fmt.Sprintf("%d", len(models)-20) + " more"
				break
			}
			display += "- " + m + "\n"
		}
		return c.Send(display)
	})

	b.Handle("/system", func(c telebot.Context) error {
		state := loadUserState(c.Chat().ID)
		userStates[c.Chat().ID] = state
		state.PendingInput = "system"
		saveUserState(c.Chat().ID, state)
		return c.Send("Send me the system prompt you want to use.")
	})

	b.Handle("/reset", func(c telebot.Context) error {
		state := loadUserState(c.Chat().ID)
		state.SystemPrompt = "You are a helpful assistant."
		saveUserState(c.Chat().ID, state)
		userStates[c.Chat().ID] = state
		return c.Send("System prompt reset to default.")
	})

	b.Handle("/clear", func(c telebot.Context) error {
		state := loadUserState(c.Chat().ID)
		state.History = nil
		saveUserState(c.Chat().ID, state)
		userStates[c.Chat().ID] = state
		return c.Send("Conversation cleared. Starting fresh!")
	})

	b.Handle("/new", func(c telebot.Context) error {
		chatID := c.Chat().ID
		
		// Delete state file entirely for a fresh start
		statePath := getStateFilePath(chatID)
		os.Remove(statePath)
		
		// Clear in-memory state
		mu.Lock()
		delete(userStates, chatID)
		mu.Unlock()
		
		return c.Send("New conversation started! All context cleared.")
	})

	// /set 1 model_name system_prompt - save a preset
	b.Handle("/set", func(c telebot.Context) error {
		// Parse manually from raw text since Args() may not work as expected
		msg := c.Message().Text
		parts := strings.Fields(strings.TrimPrefix(msg, "/set"))
		
		if len(parts) < 2 {
			return c.Send("Usage: /set <slot> <model> [system prompt]\nExample: /set 1 llama3\nExample: /set 2 glm-5 You are a coder.")
		}
		slot := parts[0]
		model := parts[1]
		systemPrompt := "You are a helpful assistant."
		if len(parts) >= 3 {
			systemPrompt = strings.Join(parts[2:], " ")
		}
		
		state := loadUserState(c.Chat().ID)
		state.Presets[slot] = Preset{Model: model, SystemPrompt: systemPrompt}
		saveUserState(c.Chat().ID, state)
		userStates[c.Chat().ID] = state
		return c.Send("Saved preset "+slot+": "+model+"\n"+systemPrompt)
	})

	// /preset - list presets, /preset <n> - load preset
	b.Handle("/preset", func(c telebot.Context) error {
		args := c.Args()
		// Handle /preset, /preset list
		if len(args) < 1 || (len(args) >= 1 && (args[0] == "list" || args[0] == "help")) {
			state := loadUserState(c.Chat().ID)
			if len(state.Presets) == 0 {
				return c.Send("No presets saved. Use /set <slot> <model> <prompt>")
			}
			msg := "Saved presets:\n"
			for k, v := range state.Presets {
				msg += "/" + k + ": " + v.Model + "\n"
			}
			return c.Send(msg)
		}
		slot := args[0]
		state := loadUserState(c.Chat().ID)
		preset, ok := state.Presets[slot]
		if !ok {
			return c.Send("Preset "+slot+" not found. Use /set to create one.")
		}
		state.Model = preset.Model
		state.SystemPrompt = preset.SystemPrompt
		saveUserState(c.Chat().ID, state)
		userStates[c.Chat().ID] = state
		return c.Send("Switched to preset "+slot+":\nModel: "+preset.Model+"\nSystem: "+preset.SystemPrompt)
	})

	// Handle text messages (not commands)
	b.Handle(telebot.OnText, func(c telebot.Context) error {
		msg := c.Message().Text
		
		// Skip commands - let command handlers deal with them
		if strings.HasPrefix(msg, "/") {
			return nil
		}
		
		// Check if waiting for model input
		if userStates[c.Chat().ID] != nil && userStates[c.Chat().ID].PendingInput == "model" {
			state := userStates[c.Chat().ID]
			state.Model = msg
			state.PendingInput = ""
			saveUserState(c.Chat().ID, state)
			return c.Send("Model set to: " + msg)
		}

		// Check if waiting for system prompt input
		if userStates[c.Chat().ID] != nil && userStates[c.Chat().ID].PendingInput == "system" {
			state := userStates[c.Chat().ID]
			state.SystemPrompt = msg
			state.PendingInput = ""
			saveUserState(c.Chat().ID, state)
			return c.Send("System prompt updated.")
		}

		// Get or create queue for this user
		mu.Lock()
		if userQueues[c.Chat().ID] == nil {
			userQueues[c.Chat().ID] = make(chan string, 10)
			// Start worker for this user
			go processMessageQueue(c.Chat().ID, c)
		}
		queue := userQueues[c.Chat().ID]
		mu.Unlock()
		
		// Queue the message (non-blocking)
		select {
		case queue <- msg:
			return nil
		default:
			return c.Send("Please wait, your previous request is still processing.")
		}
	})

	bot.Start()
}

// convertMarkdownToHTML converts basic markdown to HTML for Telegram
func convertMarkdownToHTML(text string) string {
	// Escape HTML characters first
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	// Headers
	text = regexp.MustCompile(`(?m)^###### (.+)$`).ReplaceAllString(text, "<h6>$1</h6>")
	text = regexp.MustCompile(`(?m)^##### (.+)$`).ReplaceAllString(text, "<h5>$1</h5>")
	text = regexp.MustCompile(`(?m)^#### (.+)$`).ReplaceAllString(text, "<h4>$1</h4>")
	text = regexp.MustCompile(`(?m)^### (.+)$`).ReplaceAllString(text, "<h3>$1</h3>")
	text = regexp.MustCompile(`(?m)^## (.+)$`).ReplaceAllString(text, "<h2>$1</h2>")
	text = regexp.MustCompile(`(?m)^# (.+)$`).ReplaceAllString(text, "<h1>$1</h1>")

	// Bold
	text = regexp.MustCompile(`\*\*(.+?)\*\*`).ReplaceAllString(text, "<b>$1</b>")
	text = regexp.MustCompile(`__(.+?)__`).ReplaceAllString(text, "<b>$1</b>")

	// Italic
	text = regexp.MustCompile(`\*(.+?)\*`).ReplaceAllString(text, "<i>$1</i>")
	text = regexp.MustCompile(`_(.+?)_`).ReplaceAllString(text, "<i>$1</i>")

	// Strikethrough
	text = regexp.MustCompile(`~~(.+?)~~`).ReplaceAllString(text, "<s>$1</s>")

	// Code blocks
	text = regexp.MustCompile("```(\\w+)?\\n([\\s\\S]*?)```").ReplaceAllString(text, "<code>$2</code>")
	text = regexp.MustCompile("`(.+?)`").ReplaceAllString(text, "<code>$1</code>")

	// Links
	text = regexp.MustCompile(`\[(.+?)\]\((.+?)\)`).ReplaceAllString(text, "<a href=\"$2\">$1</a>")

	// Unordered lists
	text = regexp.MustCompile(`(?m)^[\-\*] (.+)$`).ReplaceAllString(text, "• $1")

	// Ordered lists
	text = regexp.MustCompile(`(?m)^(\d+)\. (.+)$`).ReplaceAllString(text, "$1. $2")

	// Blockquotes
	text = regexp.MustCompile(`(?m)^> (.+)$`).ReplaceAllString(text, ">$1")

	return text
}

// splitAndSend splits long messages into chunks under Telegram's 4096 limit
func splitAndSend(c telebot.Context, text string) error {
	const maxLen = 4000 // Leave room for safety
	if len(text) <= maxLen {
		return c.Send(text)
	}
	
	// Split by paragraphs first, then by words if needed
	lines := strings.Split(text, "\n")
	var chunk string
	
	for _, line := range lines {
		// Handle empty lines
		if line == "" {
			chunk += "\n"
			continue
		}
		
		// If single line is too long, split by words
		if len(line) > maxLen {
			if chunk != "" {
				if err := c.Send(chunk); err != nil {
					return err
				}
				chunk = ""
			}
			words := strings.Split(line, " ")
			for _, word := range words {
				if len(chunk)+len(word)+1 > maxLen {
					if err := c.Send(chunk); err != nil {
						return err
					}
					chunk = ""
				}
				chunk += word + " "
			}
			continue
		}
		
		// Normal line
		if len(chunk)+len(line)+1 > maxLen {
			if err := c.Send(chunk); err != nil {
				return err
			}
			chunk = line
		} else {
			chunk += line + "\n"
		}
	}
	
	if chunk != "" {
		return c.Send(chunk)
	}
	return nil
}
