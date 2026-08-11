// Package runtime 持久化当前 GoPit 的运行模式，供命令恢复和开机启动使用。
package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	ModeServer = "server"
	ModeClient = "client"
)

// State 记录最近一次 start 或 join 选择的模式及对应配置文件。
type State struct {
	Mode       string `yaml:"mode"`
	ConfigPath string `yaml:"config_path"`
}

// Load 读取已持久化的运行模式。
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err := yaml.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Mode != ModeServer && state.Mode != ModeClient {
		return nil, fmt.Errorf("invalid runtime mode: %q", state.Mode)
	}
	if state.ConfigPath == "" {
		return nil, fmt.Errorf("runtime config_path is required")
	}
	return &state, nil
}

// Save 确保状态目录存在，并以当前用户可读写权限持久化运行状态。
func Save(path string, state State) error {
	data, err := yaml.Marshal(state)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if directory != "." {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0600)
}
