package mcp

import "strings"

// Provider name constants — kept in the mcp package so that client.go can
// reference them for default configuration without importing sub-packages.
// Provider sub-packages re-use these same values.
const (
	ProviderDeepSeek = "deepseek"
	ProviderOpenAI   = "openai"
	ProviderClaude   = "claude"
	ProviderQwen     = "qwen"
	ProviderGemini   = "gemini"
	ProviderGrok     = "grok"
	ProviderKimi     = "kimi"
	ProviderMiniMax  = "minimax"

	ProviderClaw402 = "claw402"

	// Default DeepSeek configuration (used as fallback in NewClient)
	DefaultDeepSeekBaseURL = "https://api.deepseek.com"
	DefaultDeepSeekModel   = "deepseek-chat"

	// Default Qwen configuration (used by WithQwenConfig convenience option)
	DefaultQwenBaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	DefaultQwenModel   = "qwen3-max"

	// Default MiniMax configuration (used by WithMiniMaxConfig convenience option)
	DefaultMiniMaxBaseURL = "https://api.minimax.io/v1"
	DefaultMiniMaxModel   = "MiniMax-M2.7"
)

// defaultProviderBaseURLs maps provider names to their OpenAI-compatible base
// URL. BuildUrl appends /chat/completions to this value (Anthropic and Gemini
// expose OpenAI-compatible endpoints under the paths below).
var defaultProviderBaseURLs = map[string]string{
	ProviderDeepSeek: "https://api.deepseek.com",
	ProviderOpenAI:   "https://api.openai.com/v1",
	ProviderClaude:   "https://api.anthropic.com/v1",
	ProviderQwen:     "https://dashscope.aliyuncs.com/compatible-mode/v1",
	ProviderGemini:   "https://generativelanguage.googleapis.com/v1beta/openai",
	ProviderGrok:     "https://api.x.ai/v1",
	ProviderKimi:     "https://api.moonshot.cn/v1",
	ProviderMiniMax:  "https://api.minimax.io/v1",
}

// defaultProviderModels maps provider names to a default model used when the
// AI_MODEL configuration is empty.
var defaultProviderModels = map[string]string{
	ProviderDeepSeek: "deepseek-chat",
	ProviderOpenAI:   "gpt-4o-mini",
	ProviderClaude:   "claude-sonnet-4-6",
	ProviderQwen:     "qwen3-max",
	ProviderGemini:   "gemini-2.5-flash",
	ProviderGrok:     "grok-3",
	ProviderKimi:     "kimi-k2",
	ProviderMiniMax:  "MiniMax-M2.7",
}

// DefaultBaseURLForProvider returns the OpenAI-compatible base URL for a
// provider, or "" when the provider is unknown.
func DefaultBaseURLForProvider(provider string) string {
	return defaultProviderBaseURLs[strings.ToLower(strings.TrimSpace(provider))]
}

// DefaultModelForProvider returns the default model for a provider, or "".
func DefaultModelForProvider(provider string) string {
	return defaultProviderModels[strings.ToLower(strings.TrimSpace(provider))]
}
