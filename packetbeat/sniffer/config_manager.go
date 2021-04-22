package sniffer

import (
	"fmt"
	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"sync/atomic"
)

const ConfigPath = "./config.yml"

type Config struct {
	Lo  Local `mapstructure:"local" json:"local" yaml:"local"`
	DB  DB    `mapstructure:"db" json:"db" yaml:"db"`
	Kfk Kafka `mapstructure:"kafka" json:"kafka" yaml:"kafka"`
}

type Local struct {
	Device        string `mapstructure:"device" json:"device" yaml:"device"`
	CpuThreshold  uint32 `mapstructure:"cpu_threshold" json:"cpu_threshold" yaml:"cpu_threshold"`
	MemThreashold uint32 `mapstructure:"mem_threshold" json:"mem_threshold" yaml:"mem_threshold"`
}

type DB struct {
	Host string `mapstructure:"host" json:"host" yaml:"host"`
	Ports []uint32 `mapstructure:"ports" json:"ports" yaml:"ports"`
}

type Kafka struct {
	Host string `mapstructure:"host" json:"host" yaml:"host"`
	Port uint32 `mapstructure:"port" json:"port" yaml:"port"`
}

type ConfigManager struct {
	switchIndex BoolIndex
	config      [2]*Config
}

// NewManager return empty Manager
func NewConfigManager() *ConfigManager {
	return &ConfigManager{
	}
}

// CreateManager create manager
func CreateConfigManager(cfg *Config) (*ConfigManager, error) {
	m := NewConfigManager()
	current, _, _ := m.switchIndex.Get()
	// init namespace
	m.config[current] = cfg
	return m, nil
}

func (m *ConfigManager) ReloadAllPrepare(
	cfg *Config) error {
	_, other, _ := m.switchIndex.Get()
	m.config[other] = cfg
	return nil
}

func (m *ConfigManager) ReloadCfgPrepare() error {
	cfg, err := loadConfig(ConfigPath)
	if err != nil {
		logp.Warn("reload config failed:%v", err)
		return err
	}

	err = m.ReloadAllPrepare(cfg)
	if err != nil {
		logp.Warn("reload manager failed:%v", err)
		return err
	}
	return nil
}

func (s *ConfigManager) ReloadCfgCommit() error {
	logp.Warn("commit config  begin")
	defer func() {
		recover()
	}()
	_, _, index := s.switchIndex.Get()

	s.switchIndex.Set(!index)
	logp.Warn("commit config end")
	return nil
}

func (m *ConfigManager) GetConfig() *Config {
	current, _, _ := m.switchIndex.Get()
	return m.config[current]
}

func (m *ConfigManager)Watch() {
	v := viper.New()
	v.SetConfigFile(ConfigPath)
	v.WatchConfig()

	v.OnConfigChange(func(e fsnotify.Event) {
		logp.Warn("config file changed:%s", e.Name)
		fmt.Println("config file changed:", e.Name)
		if m.ReloadCfgPrepare() == nil {
			m.ReloadCfgCommit()
		}
	})

}

func loadConfig(config string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(config)
	v.SetConfigType("yaml")
	err := v.ReadInConfig()
	if err != nil {
		panic(fmt.Errorf("Fatal error config file: %s \n", err))
	}
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// BoolIndex rolled array switch mark
type BoolIndex struct {
	index int32
}

// Set set index value
func (b *BoolIndex) Set(index bool) {
	if index {
		atomic.StoreInt32(&b.index, 1)
	} else {
		atomic.StoreInt32(&b.index, 0)
	}
}

// Get return current, next, current bool value
func (b *BoolIndex) Get() (int32, int32, bool) {
	index := atomic.LoadInt32(&b.index)
	if index == 1 {
		return 1, 0, true
	}
	return 0, 1, false
}
