package zabbixbridge

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const maxSecretBytes = 64 << 10

type Config struct {
	GatewayURL        string        `yaml:"gateway_url"`
	GatewayCAFile     string        `yaml:"gateway_ca_file"`
	GatewayTokenFile  string        `yaml:"gateway_token_file"`
	ZabbixAPIURL      string        `yaml:"zabbix_api_url"`
	ZabbixCAFile      string        `yaml:"zabbix_ca_file"`
	ZabbixTokenFile   string        `yaml:"zabbix_token_file"`
	ZabbixHost        string        `yaml:"zabbix_host"`
	PollInterval      time.Duration `yaml:"-"`
	InventoryInterval time.Duration `yaml:"-"`
	RequestTimeout    time.Duration `yaml:"-"`
	CursorStatePath   string        `yaml:"cursor_state_path"`
	BatchSize         int           `yaml:"batch_size"`
	MaxPages          int           `yaml:"max_pages"`
	MaxResponseBytes  int64         `yaml:"max_response_bytes"`
	gatewayToken      string
	zabbixToken       string
}

type configWire struct {
	GatewayURL        string `yaml:"gateway_url"`
	GatewayCAFile     string `yaml:"gateway_ca_file"`
	GatewayTokenFile  string `yaml:"gateway_token_file"`
	ZabbixAPIURL      string `yaml:"zabbix_api_url"`
	ZabbixCAFile      string `yaml:"zabbix_ca_file"`
	ZabbixTokenFile   string `yaml:"zabbix_token_file"`
	ZabbixHost        string `yaml:"zabbix_host"`
	PollInterval      string `yaml:"poll_interval"`
	InventoryInterval string `yaml:"inventory_interval"`
	RequestTimeout    string `yaml:"request_timeout"`
	CursorStatePath   string `yaml:"cursor_state_path"`
	BatchSize         int    `yaml:"batch_size"`
	MaxPages          int    `yaml:"max_pages"`
	MaxResponseBytes  int64  `yaml:"max_response_bytes"`
}

func LoadConfig(path string) (Config, error) { return loadConfig(path) }

func loadConfig(path string) (Config, error) {
	f, err := openProtectedRegular(path, "configuration")
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	closeErr := f.Close()
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if closeErr != nil {
		return Config{}, fmt.Errorf("close config: %w", closeErr)
	}
	if len(data) > 1<<20 {
		return Config{}, fmt.Errorf("config exceeds 1 MiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var w configWire
	if err := dec.Decode(&w); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return Config{}, fmt.Errorf("decode config: multiple documents")
	} else if err != io.EOF {
		return Config{}, fmt.Errorf("decode trailing config: %w", err)
	}
	cfg := Config{GatewayURL: w.GatewayURL, GatewayCAFile: w.GatewayCAFile, GatewayTokenFile: w.GatewayTokenFile, ZabbixAPIURL: w.ZabbixAPIURL, ZabbixCAFile: w.ZabbixCAFile, ZabbixTokenFile: w.ZabbixTokenFile, ZabbixHost: w.ZabbixHost, CursorStatePath: w.CursorStatePath, BatchSize: w.BatchSize, MaxPages: w.MaxPages, MaxResponseBytes: w.MaxResponseBytes}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxPages == 0 {
		cfg.MaxPages = 100
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = 8 << 20
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > 1000 || cfg.MaxPages < 1 || cfg.MaxPages > 10000 || cfg.MaxResponseBytes < 1024 || cfg.MaxResponseBytes > 64<<20 {
		return Config{}, fmt.Errorf("invalid bounded batch/page/response settings")
	}
	if cfg.PollInterval, err = durationDefault(w.PollInterval, 5*time.Second); err != nil || cfg.PollInterval < 5*time.Second {
		return Config{}, fmt.Errorf("poll_interval must be at least 5s")
	}
	if cfg.InventoryInterval, err = durationDefault(w.InventoryInterval, 15*time.Minute); err != nil || cfg.InventoryInterval < 5*time.Second {
		return Config{}, fmt.Errorf("inventory_interval must be at least 5s")
	}
	if cfg.RequestTimeout, err = durationDefault(w.RequestTimeout, 10*time.Second); err != nil || cfg.RequestTimeout <= 0 {
		return Config{}, fmt.Errorf("invalid request_timeout")
	}
	for name, raw := range map[string]string{"gateway_url": cfg.GatewayURL, "zabbix_api_url": cfg.ZabbixAPIURL} {
		u, parseErr := url.Parse(raw)
		if parseErr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return Config{}, fmt.Errorf("%s must be an HTTPS URL without credentials, query, or fragment", name)
		}
	}
	if cfg.GatewayTokenFile == "" || cfg.ZabbixTokenFile == "" || cfg.ZabbixHost == "" || cfg.CursorStatePath == "" {
		return Config{}, fmt.Errorf("required configuration is missing")
	}
	if !filepath.IsAbs(cfg.CursorStatePath) {
		return Config{}, fmt.Errorf("cursor_state_path must be absolute")
	}
	if cfg.gatewayToken, err = readSecret(cfg.GatewayTokenFile); err != nil {
		return Config{}, fmt.Errorf("read gateway token: %w", err)
	}
	if cfg.zabbixToken, err = readSecret(cfg.ZabbixTokenFile); err != nil {
		return Config{}, fmt.Errorf("read zabbix token: %w", err)
	}
	return cfg, nil
}

func durationDefault(value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	return time.ParseDuration(value)
}

func readSecret(path string) (string, error) {
	f, err := openProtectedRegular(path, "secret")
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", err
	}
	if len(buf) > maxSecretBytes {
		return "", fmt.Errorf("secret file too large")
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(buf), "\n"), "\r")
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < 0x21 || r == 0x7f }) >= 0 {
		return "", fmt.Errorf("secret is empty or contains controls/whitespace")
	}
	return value, nil
}

func openProtectedRegular(path, purpose string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open %s", purpose)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	permissions := info.Mode().Perm()
	ownerOK := ok && (stat.Uid == 0 || stat.Uid == uint32(os.Geteuid()))
	groupOK := permissions&0o040 == 0 || ok && stat.Gid == uint32(os.Getegid())
	if !info.Mode().IsRegular() || permissions&0o137 != 0 || !ownerOK || !groupOK || stat.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("%s file must be regular, singly linked, owned by root or the service user, and mode 0640 or stricter for the service group", purpose)
	}
	return f, nil
}

func (c Config) String() string {
	return fmt.Sprintf("gateway=%s zabbix=%s host=%s", c.GatewayURL, c.ZabbixAPIURL, c.ZabbixHost)
}
