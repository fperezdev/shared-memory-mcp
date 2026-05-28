// Package config loads and validates the per-device configuration.
//
// The config lives at:
//
//	~/.config/shared-memory-mcp/config.json    (Unix)
//	%APPDATA%\shared-memory-mcp\config.json    (Windows)
//
// Override the directory with SHARED_MEMORY_MCP_CONFIG_DIR. Individual
// fields can be overridden by env vars (see envOverride below).
//
// The file holds the runtime credentials (scoped Postgres role connection
// string + a per-device id) so we treat it as sensitive: if it's group- or
// world-readable on Unix, or owned by another user on Windows, we refuse
// to start.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// utf8BOM is what Notepad and Windows PowerShell 5.1 prepend to "UTF-8"
// files. encoding/json doesn't strip it; we do.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

type Config struct {
	DB      DBConfig      `json:"db"`
	Device  DeviceConfig  `json:"device"`
	Project ProjectConfig `json:"project"`
	Sync    SyncConfig    `json:"sync"`
}

type DBConfig struct {
	ConnectionString string `json:"connectionString"`
	CACertPath       string `json:"caCertPath"`
}

type DeviceConfig struct {
	ID string `json:"id"`
}

type ProjectConfig struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type SyncConfig struct {
	IntervalSeconds int    `json:"intervalSeconds"`
	PageSize        int    `json:"pageSize"`
	LocalDBPath     string `json:"localDbPath"`
}

// Paths returns the resolved config directory and file path for this OS,
// honoring SHARED_MEMORY_MCP_CONFIG_DIR if set.
func Paths() (dir, file string) {
	if override := os.Getenv("SHARED_MEMORY_MCP_CONFIG_DIR"); override != "" {
		return override, filepath.Join(override, "config.json")
	}
	if runtime.GOOS == "windows" {
		root := os.Getenv("APPDATA")
		if root == "" {
			home, _ := os.UserHomeDir()
			root = filepath.Join(home, "AppData", "Roaming")
		}
		dir = filepath.Join(root, "shared-memory-mcp")
	} else {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "shared-memory-mcp")
	}
	return dir, filepath.Join(dir, "config.json")
}

// DefaultLocalDBPath returns the default SQLite cache location for this OS.
func DefaultLocalDBPath() string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("LOCALAPPDATA")
		if root == "" {
			home, _ := os.UserHomeDir()
			root = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(root, "shared-memory-mcp", "local.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "shared-memory-mcp", "local.db")
}

// Load reads the config file, validates perms, applies env overrides,
// and validates required fields.
func Load() (*Config, error) {
	_, path := Paths()

	c := &Config{}
	if data, err := os.ReadFile(path); err == nil {
		if err := assertSecurePerms(path); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	envOverride(c)
	applyDefaults(c)

	if c.Device.ID == "" {
		return nil, fmt.Errorf("missing device.id (set in %s or via SHARED_MEMORY_DEVICE_ID); generate one with `uuidgen` on Unix or `[guid]::NewGuid()` in PowerShell", path)
	}
	// db.connectionString is optional: empty means local-only mode.
	return c, nil
}

func envOverride(c *Config) {
	if v := os.Getenv("SHARED_MEMORY_DB_URL"); v != "" {
		c.DB.ConnectionString = v
	}
	if v := os.Getenv("SHARED_MEMORY_CA_CERT_PATH"); v != "" {
		c.DB.CACertPath = v
	}
	if v := os.Getenv("SHARED_MEMORY_DEVICE_ID"); v != "" {
		c.Device.ID = v
	}
	if v := os.Getenv("SHARED_MEMORY_PROJECT_SLUG"); v != "" {
		c.Project.Slug = v
	}
	if v := os.Getenv("SHARED_MEMORY_PROJECT_NAME"); v != "" {
		c.Project.Name = v
	}
	if v := os.Getenv("SHARED_MEMORY_SYNC_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Sync.IntervalSeconds = n
		}
	}
	if v := os.Getenv("SHARED_MEMORY_LOCAL_DB"); v != "" {
		c.Sync.LocalDBPath = v
	}
}

func applyDefaults(c *Config) {
	if c.Sync.IntervalSeconds <= 0 {
		c.Sync.IntervalSeconds = 60
	}
	if c.Sync.PageSize <= 0 {
		c.Sync.PageSize = 1000
	}
	if c.Sync.LocalDBPath == "" {
		c.Sync.LocalDBPath = DefaultLocalDBPath()
	}
}

// assertSecurePerms refuses to start if the config file is readable by
// principals other than the current user. On Unix it checks mode bits; on
// Windows it best-effort verifies the file owner matches the current user.
func assertSecurePerms(path string) error {
	if runtime.GOOS == "windows" {
		return assertSecurePermsWindows(path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config file %s is group/world-readable (mode %04o). Fix with: chmod 600 %s",
			path, st.Mode().Perm(), path)
	}
	return nil
}

func assertSecurePermsWindows(path string) error {
	// Get-Acl is the canonical way to read file ACLs from PowerShell.
	// Compare the owner SID/name against the current user; if it doesn't
	// match, treat as compromised.
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(`(Get-Acl '%s').Owner`, strings.ReplaceAll(path, "'", "''"))).Output()
	if err != nil {
		// Best-effort: if we can't read the ACL, don't block.
		return nil
	}
	owner := strings.TrimSpace(string(out))
	domain := os.Getenv("USERDOMAIN")
	user := os.Getenv("USERNAME")
	if user == "" {
		return nil
	}
	expected := user
	if domain != "" {
		expected = domain + "\\" + user
	}
	if owner != "" && !strings.EqualFold(owner, expected) {
		return fmt.Errorf(`config file %s is owned by %q but current user is %q. `+
			`Recreate it under your account, or fix ACLs with: `+
			`icacls "%s" /inheritance:r /grant:r "%s:F"`,
			path, owner, expected, path, expected)
	}
	return nil
}
