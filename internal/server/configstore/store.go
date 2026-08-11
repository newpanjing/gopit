package configstore

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopit/internal/config"
)

// Store 是服务端配置的线程安全存储，支持原子写入和文件监听。
type Store struct {
	mu         sync.RWMutex
	cfg        *config.ServerConfig
	configPath string
	logger     *slog.Logger

	writing atomic.Bool // 标记自身正在写入，用于忽略文件监听事件
	stopCh  chan struct{}
	stopped atomic.Bool

	// 回调：配置成功热加载后调用
	OnConfigReloaded func(newCfg *config.ServerConfig)
}

// NewStore 从指定路径加载配置并创建 Store。
func NewStore(path string, logger *slog.Logger) (*Store, error) {
	cfg, err := config.LoadServerConfig(path)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	s := &Store{
		cfg:        cfg,
		configPath: path,
		logger:     logger,
		stopCh:     make(chan struct{}),
	}
	return s, nil
}

// Get 返回当前配置的副本。
func (s *Store) Get() *config.ServerConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cfg)
}

// GetConfigVersion 返回当前配置版本。
func (s *Store) GetConfigVersion() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Server.ConfigVersion
}

// Save 原子写入新配置到文件并更新内存。
// 调用者应确保 newCfg 已通过 Validate 校验。
func (s *Store) Save(newCfg *config.ServerConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 递增配置版本
	newCfg.Server.ConfigVersion = s.cfg.Server.ConfigVersion + 1

	// 原子写入文件
	s.writing.Store(true)
	defer s.writing.Store(false)

	if err := atomicWriteFile(s.configPath, newCfg); err != nil {
		return fmt.Errorf("atomic write config: %w", err)
	}

	s.cfg = newCfg
	s.logger.Info("config saved", "version", newCfg.Server.ConfigVersion)
	return nil
}

// Update 在锁保护下修改配置并原子写入。
// updateFn 接收当前配置的副本，返回修改后的配置。
func (s *Store) Update(updateFn func(cfg *config.ServerConfig) (*config.ServerConfig, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	working := cloneConfig(s.cfg)
	newCfg, err := updateFn(working)
	if err != nil {
		return err
	}

	if err := Validate(newCfg); err != nil {
		return fmt.Errorf("validation: %w", err)
	}

	newCfg.Server.ConfigVersion = s.cfg.Server.ConfigVersion + 1

	s.writing.Store(true)
	defer s.writing.Store(false)

	if err := atomicWriteFile(s.configPath, newCfg); err != nil {
		return fmt.Errorf("atomic write config: %w", err)
	}

	s.cfg = newCfg
	s.logger.Info("config updated", "version", newCfg.Server.ConfigVersion)
	return nil
}

// StartWatcher 启动配置文件变更监听（基于轮询）。
func (s *Store) StartWatcher(interval time.Duration) {
	go s.watchLoop(interval)
}

func (s *Store) watchLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastMod time.Time
	if fi, err := os.Stat(s.configPath); err == nil {
		lastMod = fi.ModTime()
	}

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			// 忽略自身写入产生的事件
			if s.writing.Load() {
				continue
			}

			fi, err := os.Stat(s.configPath)
			if err != nil {
				s.logger.Warn("stat config file failed", "err", err)
				continue
			}

			if fi.ModTime().After(lastMod) {
				lastMod = fi.ModTime()
				s.reloadFromFile()
			}
		}
	}
}

func (s *Store) reloadFromFile() {
	newCfg, err := config.LoadServerConfig(s.configPath)
	if err != nil {
		s.logger.Error("reload config failed, keeping current", "err", err)
		return
	}

	if err := Validate(newCfg); err != nil {
		s.logger.Error("reload config validation failed, keeping current", "err", err)
		return
	}

	s.mu.Lock()
	// 如果文件中的版本未递增，自动递增
	if newCfg.Server.ConfigVersion <= s.cfg.Server.ConfigVersion {
		newCfg.Server.ConfigVersion = s.cfg.Server.ConfigVersion + 1
	}
	s.cfg = newCfg
	s.mu.Unlock()

	s.logger.Info("config reloaded from file", "version", newCfg.Server.ConfigVersion)

	if s.OnConfigReloaded != nil {
		s.OnConfigReloaded(newCfg)
	}
}

// Close 停止文件监听。
func (s *Store) Close() {
	if s.stopped.CompareAndSwap(false, true) {
		close(s.stopCh)
	}
}

// --- 原子写入 ---

func atomicWriteFile(path string, cfg *config.ServerConfig) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".server-yaml-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // 如果 rename 成功，tmpPath 已不存在

	if err := config.SaveServerConfig(tmpPath, cfg); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()

	// fsync 确保数据落盘
	f, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// 原子重命名
	return os.Rename(tmpPath, path)
}

// --- 配置副本 ---

func cloneConfig(cfg *config.ServerConfig) *config.ServerConfig {
	out := *cfg
	if cfg.Connections != nil {
		out.Connections = make([]config.Connection, len(cfg.Connections))
		copy(out.Connections, cfg.Connections)
	}
	return &out
}
