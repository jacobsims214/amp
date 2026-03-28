package config

import (
	"fmt"
	"github.com/spf13/viper"
)

// Config holds all application configuration
type Config struct {
	Server   ServerConfig `mapstructure:"server"`
	Odoo     OdooConfig   `mapstructure:"odoo"`
	LogLevel string       `mapstructure:"log_level"`
}

// ServerConfig holds server configuration
type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

// OdooConfig holds Odoo configuration
type OdooConfig struct {
	URL      string `mapstructure:"url"`
	DB       string `mapstructure:"db"`
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	viper.SetEnvPrefix("amp")
	viper.AutomaticEnv()

	// Set defaults
	viper.SetDefault("server.host", "0.0.0.0")
	viper.SetDefault("server.port", 8000)
	viper.SetDefault("odoo.url", "http://host.docker.internal:8069")
	viper.SetDefault("odoo.db", "odoo19")
	viper.SetDefault("odoo.username", "admin")
	viper.SetDefault("odoo.password", "admin")
	viper.SetDefault("log_level", "info")

	// Map environment variables with specific names
	viper.BindEnv("odoo.url", "ODOO_URL")
	viper.BindEnv("odoo.db", "ODOO_DB")
	viper.BindEnv("odoo.username", "ODOO_USER")
	viper.BindEnv("odoo.password", "ODOO_PASSWORD")
	viper.BindEnv("server.port", "MCP_PORT")
	viper.BindEnv("log_level", "LOG_LEVEL")

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &cfg, nil
}

// GetAddress returns the server address
func (c *ServerConfig) GetAddress() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}
