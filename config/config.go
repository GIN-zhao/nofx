package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Global configuration instance
var global *Config

// Config is the global configuration (loaded from .env / environment).
// Trading parameters (risk controls, indicators, prompts) live in the
// strategy row in the database; this struct only holds process-level config.
type Config struct {
	// Database (SQLite)
	DBType string // sqlite
	DBPath string // SQLite database file path

	// AI strategy brain (LLM)
	AIProvider string // deepseek / openai / claude / qwen / gemini / grok / kimi / minimax
	AIModel    string // optional; empty = provider default
	AIAPIKey   string
	AIBaseURL  string // optional; OpenAI-compatible base URL (e.g. https://open.bigmodel.cn/api/paas/v4)

	// Telegram notifier
	TelegramBotToken string
	TelegramChatID   string

	// Paper trading
	InitialBalance float64 // simulated account principal (USDT)
	ScanInterval   int     // seconds between trading cycles

	// Market data
	OKXBaseURL string // OKX public REST base URL
}

// MustInit initializes global configuration or panics.
func MustInit() {
	if err := initConfig(); err != nil {
		panic(fmt.Sprintf("config: %v", err))
	}
}

// Init initializes global configuration (from .env). Prefer MustInit from main.
func Init() {
	if err := initConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "config init failed: %v\n", err)
	}
}

// providerAPIKeyEnv maps provider names to their conventional API key env var.
var providerAPIKeyEnv = map[string]string{
	"deepseek": "DEEPSEEK_API_KEY",
	"openai":   "OPENAI_API_KEY",
	"claude":   "ANTHROPIC_API_KEY",
	"qwen":     "QWEN_API_KEY",
	"gemini":   "GEMINI_API_KEY",
	"grok":     "GROK_API_KEY",
	"kimi":     "KIMI_API_KEY",
	"minimax":  "MINIMAX_API_KEY",
}

func initConfig() error {
	cfg := &Config{
		DBType:         "sqlite",
		DBPath:         "data/data.db",
		AIProvider:     "deepseek",
		InitialBalance: 1000,
		ScanInterval:   300,
		OKXBaseURL:     "https://www.okx.com",
	}

	if v := os.Getenv("DB_TYPE"); v != "" {
		cfg.DBType = strings.ToLower(v)
	}
	if v := os.Getenv("DB_PATH"); v != "" {
		cfg.DBPath = v
	}

	// AI provider / model / key
	if v := os.Getenv("AI_PROVIDER"); v != "" {
		cfg.AIProvider = strings.ToLower(strings.TrimSpace(v))
	}
	cfg.AIModel = strings.TrimSpace(os.Getenv("AI_MODEL"))
	cfg.AIBaseURL = strings.TrimSpace(os.Getenv("AI_BASE_URL"))
	cfg.AIAPIKey = strings.TrimSpace(os.Getenv("AI_API_KEY"))
	if cfg.AIAPIKey == "" {
		// Fall back to provider-specific conventional env vars
		if envName, ok := providerAPIKeyEnv[cfg.AIProvider]; ok {
			cfg.AIAPIKey = strings.TrimSpace(os.Getenv(envName))
		}
	}

	// Telegram notifier
	cfg.TelegramBotToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	cfg.TelegramChatID = strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))

	// Paper trading
	if v := os.Getenv("INITIAL_BALANCE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			cfg.InitialBalance = f
		}
	}
	if v := os.Getenv("SCAN_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 30 {
			cfg.ScanInterval = n
		}
	}

	if v := os.Getenv("OKX_BASE_URL"); v != "" {
		cfg.OKXBaseURL = strings.TrimRight(v, "/")
	}

	if cfg.AIAPIKey == "" {
		return fmt.Errorf("AI_API_KEY is required (or set the provider key e.g. DEEPSEEK_API_KEY; AI_PROVIDER=%s)", cfg.AIProvider)
	}

	global = cfg
	return nil
}

// Get returns the global configuration
func Get() *Config {
	if global == nil {
		Init()
	}
	return global
}
