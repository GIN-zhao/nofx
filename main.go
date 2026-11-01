package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"nofx/config"
	"nofx/logger"
	"nofx/mcp"
	"nofx/notify"
	"nofx/security"
	"nofx/store"
	"nofx/trader"
	"nofx/trader/paper"

	// Register protocol-specific providers (claude = Anthropic Messages API, …)
	_ "nofx/mcp/provider"

	"github.com/joho/godotenv"
)

const paperTraderID = "paper"

func main() {
	// Load .env environment variables
	_ = godotenv.Load()

	// Outbound proxy (PROXY_URL, NO_PROXY) for OKX market data and Telegram
	// push. The AI (mcp) client picks it up via security.SafeHTTPClient.
	if f := security.ProxyFunc(); f != nil {
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			t.Proxy = f
		}
	}

	// Initialize logger
	logger.Init(nil)

	logger.Info("╔════════════════════════════════════════════════════════════╗")
	logger.Info("║   🚀 NOFX - OKX 行情 + AI 决策 + 本地模拟交易 + Telegram     ║")
	logger.Info("╚════════════════════════════════════════════════════════════╝")

	config.MustInit()
	cfg := config.Get()
	logger.Info("✅ Configuration loaded")

	// Ensure the SQLite data directory exists
	if dir := filepath.Dir(cfg.DBPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			logger.Warnf("Failed to create data directory: %v", err)
		}
	}

	st, err := store.New(cfg.DBPath)
	if err != nil {
		logger.Fatalf("❌ Failed to initialize database: %v", err)
	}
	defer st.Close()
	logger.Info("✅ Database initialized")

	// Ensure the single paper trader row exists (initial balance from config)
	traderRow, err := ensurePaperTrader(st, cfg.InitialBalance)
	if err != nil {
		logger.Fatalf("❌ Failed to prepare paper trader: %v", err)
	}
	logger.Infof("✅ Paper trader ready: initial balance %.2f USDT", traderRow.InitialBalance)

	// Strategy: prefer the stored default strategy, else the built-in default.
	strategy := store.GetDefaultStrategyConfig("en")
	if s, err := st.Strategy().GetDefault(); err == nil && s != nil {
		if parsed, err := s.ParseConfig(); err == nil {
			strategy = *parsed
		}
	}
	strategy.ClampLimits()

	// AI client — registered providers use their protocol-specific client
	// (e.g. claude = Anthropic Messages API); unknown providers fall back to
	// the generic OpenAI-compatible client.
	aiOpts := []mcp.ClientOption{
		mcp.WithProvider(cfg.AIProvider),
		mcp.WithAPIKey(cfg.AIAPIKey),
		mcp.WithModel(cfg.AIModel),
		mcp.WithBaseURL(cfg.AIBaseURL),
	}
	aiClient := mcp.NewAIClientByProvider(cfg.AIProvider, aiOpts...)
	if aiClient == nil {
		aiClient = mcp.NewClient(aiOpts...)
	}
	logger.Infof("✅ AI client ready: provider=%s model=%s", cfg.AIProvider, cfg.AIModel)

	// Paper broker + Telegram notifier + auto trader
	broker := paper.New(st, paperTraderID)
	notifier := notify.NewTelegram(cfg.TelegramBotToken, cfg.TelegramChatID)
	if !notifier.Enabled() {
		logger.Warnf("⚠️ TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID not set — notifications disabled")
	}

	autoTrader := trader.NewAutoTrader(trader.Config{
		TraderID:     paperTraderID,
		Store:        st,
		Strategy:     &strategy,
		ScanInterval: time.Duration(cfg.ScanInterval) * time.Second,
		Notifier:     notifier,
		AIClient:     aiClient,
	}, broker)
	if err := autoTrader.Start(); err != nil {
		logger.Fatalf("❌ Failed to start auto trader: %v", err)
	}

	// Telegram two-way: listen for commands/questions from the configured chat.
	if notifier.Enabled() {
		go notifier.PollMessages(autoTrader.HandleTelegramMessage)
		logger.Info("✅ Telegram 指令监听已启动 (发送 /help 查看可用指令)")
	}

	logger.Info("✅ System started, waiting for shutdown signal (Ctrl+C to stop)")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("📴 Shutdown signal received, stopping...")
	autoTrader.Stop()
	logger.Info("✅ System shut down safely")
}

// ensurePaperTrader creates or repairs the single paper trader row.
func ensurePaperTrader(st *store.Store, initialBalance float64) (*store.Trader, error) {
	row, err := st.Trader().GetByID(paperTraderID)
	if err == nil && row != nil {
		if row.InitialBalance <= 0 {
			row.InitialBalance = initialBalance
			if err := st.Trader().UpdateInitialBalance(paperTraderID, initialBalance); err != nil {
				return nil, err
			}
			row.InitialBalance = initialBalance
		}
		return row, nil
	}

	row = &store.Trader{
		ID:                  paperTraderID,
		Name:                "OKX 模拟交易",
		InitialBalance:      initialBalance,
		ScanIntervalMinutes: 5,
		IsRunning:           true,
		IsCrossMargin:       true,
	}
	if err := st.Trader().Create(row); err != nil {
		return nil, fmt.Errorf("create paper trader: %w", err)
	}
	return row, nil
}
