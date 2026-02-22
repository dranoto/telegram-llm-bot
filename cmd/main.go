package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/telebot.v3"
)

var (
	logger      = slog.Default().With(slog.String("package", "main"))
	httpClient = &http.Client{}
	bot        *telebot.Bot
)

// Config
type Config struct {
	APIToken     string `mapstructure:"api_token"`     // Telegram bot token
	APIEndpoint  string `mapstructure:"api_endpoint"`  // OpenAI-compatible endpoint
	APIKey       string `mapstructure:"api_key"`      // API key for the LLM
	DefaultModel string `mapstructure:"default_model"` // Default model
}

// User state
type UserState struct {
	Model        string                   `json:"model"`
	SystemPrompt string                   `json:"system_prompt"`
	History      []ChatMessage            `json:"history"`
	Presets      map[string]Preset       `json:"presets"`
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
	return string(rune(i))
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

	reqBody := ChatRequest{
		Model:    state.Model,
		Messages: messages,
		Stream:   false,
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

func main() {
	// Setup logging
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	logger = slog.Default().With(slog.String("package", "main"))

	// Load config
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("config")
	viper.AddConfigPath("data/config")
	viper.ReadInConfig()

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

	// Commands
	b.Handle("/start", func(c telebot.Context) error {
		state := loadUserState(c.Chat().ID)
		userStates[c.Chat().ID] = state
		return c.Send("Welcome! I'm your AI assistant.\n\nCurrent model: "+state.Model+"\n\nCommands:\n/model - Switch model\n/models - List models\n/set <n> <model> <prompt> - Save preset\n/preset - List presets\n/preset <n> - Load preset\n/new - New conversation\n/reset - Reset system prompt")
	})

	b.Handle("/model", func(c telebot.Context) error {
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
				display += "\n...and " + string(rune(len(models)-20)+'0') + " more"
				break
			}
			display += "- " + m + "\n"
		}
		return c.Send(display)
	})

	b.Handle("/system", func(c telebot.Context) error {
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
		state := loadUserState(c.Chat().ID)
		state.History = nil
		saveUserState(c.Chat().ID, state)
		userStates[c.Chat().ID] = state
		return c.Send("New conversation started!")
	})

	// /set 1 model_name system_prompt - save a preset
	b.Handle("/set", func(c telebot.Context) error {
		args := c.Args()
		if len(args) < 3 {
			return c.Send("Usage: /set <slot> <model> <system prompt>\nExample: /set 1 llama3 You are a helpful assistant.")
		}
		slot := args[0]
		model := args[1]
		systemPrompt := args[2]
		
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
		state.History = nil // Clear history when switching
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
		
		// Check if waiting for model
		if userStates[c.Chat().ID] != nil && userStates[c.Chat().ID].Model == "" {
			state := userStates[c.Chat().ID]
			state.Model = msg
			saveUserState(c.Chat().ID, state)
			return c.Send("Model set to: " + msg)
		}

		// Check if waiting for system prompt
		if userStates[c.Chat().ID] != nil && userStates[c.Chat().ID].SystemPrompt == "" {
			state := userStates[c.Chat().ID]
			state.SystemPrompt = msg
			saveUserState(c.Chat().ID, state)
			return c.Send("System prompt updated.")
		}

		// Regular chat - show typing indicator
		c.Send(telebot.Typing)
		
		response, err := sendChat(c.Chat().ID, msg)
		if err != nil {
			return c.Send("Error: " + err.Error())
		}
		
		if response == "" {
			return c.Send("No response received.")
		}
		
		// Send with markdown mode
		return c.Send(response, telebot.ModeMarkdown)
	})
}
