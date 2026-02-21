# Telegram LLM Bot

A simple Telegram bot that connects directly to any OpenAI-compatible LLM API. No middleman like OpenWebUI required.

## Features

- Direct Telegram → LLM API bridge
- Works with any OpenAI-compatible API (Ollama, nano-gpt, LM Studio, etc.)
- User state persistence (model choice, system prompt saved to disk)
- Simple commands

## Setup

1. **Create config.yaml:**
```yaml
api_token: "YOUR_TELEGRAM_BOT_TOKEN"       # From @BotFather
api_endpoint: "https://your-llm-api.com/v1" # OpenAI-compatible endpoint
api_key: "YOUR_API_KEY"                     # Your API key
default_model: "model-name"                 # Default model to use
```

2. **Run with Docker:**
```bash
docker compose up -d
```

## Commands

- `/start` - Start the bot
- `/models` - List available models from the API
- `/model` - Switch to a different model
- `/system` - Set a custom system prompt
- `/reset` - Reset system prompt to default

## Usage

Just send a message to the bot and it will respond using the configured LLM.

## Example Config (nano-gpt)

```yaml
api_token: "123456789:ABCdefGHIjklMNOpqrsTUVwxyz"
api_endpoint: "https://nano-gpt.com/api/v1"
api_key: "sk-nano-xxxxxxxxxxxxxxxxxxxx"
default_model: "qwen/qwen3.5-397b-a17b-thinking"
```
