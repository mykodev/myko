package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen string `yaml:"listen"`

	DataConfig DataConfig `yaml:"data"`

	FlushConfig FlushConfig `yaml:"flush"`
}

func DefaultConfig() Config {
	return Config{
		Listen: ":6959",
		DataConfig: DataConfig{
			CassandraConfig: CassandraConfig{
				Keyspace: "myko",
				Peers:    []string{"localhost:9042"},
			},
		},
		FlushConfig: FlushConfig{
			Interval: 5 * time.Second,
		},
	}
}

type DataConfig struct {
	CassandraConfig CassandraConfig `yaml:"cassandra"`
}

type CassandraConfig struct {
	Keyspace string `yaml:"keyspace,omitempty"`

	Peers []string `yaml:"peers,omitempty"`

	Username string `yaml:"username,omitempty"`

	Password string `yaml:"password,omitempty"`

	Datacenter string `yaml:"dc,omitempty"`

	Timeout time.Duration `yaml:"timeout,omitempty"`
}

type FlushConfig struct {
	Interval time.Duration `yaml:"interval"`
}

func Open(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	config := DefaultConfig()
	if err := yaml.NewDecoder(f).Decode(&config); err != nil {
		return Config{}, err
	}
	return config, nil
}
