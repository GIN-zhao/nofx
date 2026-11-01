package store

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// TraderStore trader storage
type TraderStore struct {
	db *gorm.DB
}

// NewTraderStore creates a new trader store
func NewTraderStore(db *gorm.DB) *TraderStore {
	return &TraderStore{db: db}
}

// Trader trader configuration (single-instance paper trader)
type Trader struct {
	ID                  string    `gorm:"primaryKey" json:"id"`
	Name                string    `gorm:"column:name;not null" json:"name"`
	StrategyID          string    `gorm:"column:strategy_id;default:''" json:"strategy_id"`
	InitialBalance      float64   `gorm:"column:initial_balance;not null" json:"initial_balance"`
	ScanIntervalMinutes int       `gorm:"column:scan_interval_minutes;default:15" json:"scan_interval_minutes"`
	IsRunning           bool      `gorm:"column:is_running;default:false" json:"is_running"`
	IsCrossMargin       bool      `gorm:"column:is_cross_margin;default:true" json:"is_cross_margin"`
	CreatedAt           time.Time `gorm:"column:created_at;autoCreateTime" json:"created_at"`
	UpdatedAt           time.Time `gorm:"column:updated_at;autoUpdateTime" json:"updated_at"`
}

// TableName returns the table name for Trader
func (Trader) TableName() string {
	return "traders"
}

func (s *TraderStore) initTables() error {
	if err := s.db.AutoMigrate(&Trader{}); err != nil {
		return fmt.Errorf("failed to migrate traders table: %w", err)
	}
	return nil
}

// Create creates trader
func (s *TraderStore) Create(trader *Trader) error {
	return s.db.Create(trader).Error
}

// List gets all traders
func (s *TraderStore) List() ([]*Trader, error) {
	var traders []*Trader
	err := s.db.Order("created_at DESC").Find(&traders).Error
	if err != nil {
		return nil, err
	}
	return traders, nil
}

// GetByID gets a trader by ID
func (s *TraderStore) GetByID(traderID string) (*Trader, error) {
	var trader Trader
	err := s.db.Where("id = ?", traderID).First(&trader).Error
	if err != nil {
		return nil, err
	}
	return &trader, nil
}

// UpdateStatus updates trader running status
func (s *TraderStore) UpdateStatus(id string, isRunning bool) error {
	return s.db.Model(&Trader{}).
		Where("id = ?", id).
		Update("is_running", isRunning).Error
}

// Update persists trader configuration
func (s *TraderStore) Update(trader *Trader) error {
	updates := map[string]interface{}{
		"name":            trader.Name,
		"strategy_id":     trader.StrategyID,
		"is_cross_margin": trader.IsCrossMargin,
	}
	if trader.InitialBalance > 0 {
		updates["initial_balance"] = trader.InitialBalance
	}
	if trader.ScanIntervalMinutes > 0 {
		updates["scan_interval_minutes"] = trader.ScanIntervalMinutes
	}
	return s.db.Model(&Trader{}).
		Where("id = ?", trader.ID).
		Updates(updates).Error
}

// UpdateInitialBalance updates initial balance
func (s *TraderStore) UpdateInitialBalance(id string, newBalance float64) error {
	return s.db.Model(&Trader{}).
		Where("id = ?", id).
		Update("initial_balance", newBalance).Error
}

// Delete deletes trader and associated equity snapshots
func (s *TraderStore) Delete(id string) error {
	s.db.Where("trader_id = ?", id).Delete(&EquitySnapshot{})
	return s.db.Where("id = ?", id).Delete(&Trader{}).Error
}
