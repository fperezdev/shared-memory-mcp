// Package config loads and validates the per-device configuration.
//
// Resolution order for the config file path:
//
//  1. SHARED_MEMORY_MCP_CONFIG_DIR env var.
//  2. ~/.config/shared-memory-mcp/config.json (Unix) or
//     %APPDATA%\shared-memory-mcp\config.json (Windows).
//
// Individual fields can be overridden by env vars (see envOverride).
//
// On first run (file missing), Load writes a template config with a
// fresh device.id and a placeholder connection string, then returns
// ErrNotConfigured so the server can exit with a clear "go edit this
// file" message.
//
// The file holds the runtime credentials (scoped Postgres role connection
// string + a per-device id) so we treat it as sensitive: if it's group-
// or world-readable on Unix, or owned by another user on Windows, we
// refuse to start.
package config

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// cryptoRandRead is split out so the platform-conditional fallback in
// newDeviceID stays readable.
func cryptoRandRead(b []byte) (int, error) { return cryptorand.Read(b) }

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
// ConnectionPlaceholder is the literal string written into a freshly
// bootstrapped config.json for db.connectionString. Load treats any
// value equal to this as "not yet configured".
const ConnectionPlaceholder = "REPLACE_WITH_OUTPUT_FROM_shared-memory-mcp-admin_init"

// ErrNotConfigured is returned by Load on first run, after a template
// config file has been written to disk. Callers should print its
// message and exit cleanly so the user knows what to do next.
type ErrNotConfigured struct {
	Path     string
	Bootstrap bool // true when we just wrote the template ourselves
}

func (e *ErrNotConfigured) Error() string {
	if e.Bootstrap {
		return fmt.Sprintf(
			"first run: wrote template config to %s.\n"+
				"  1. Run `shared-memory-mcp-admin init` (against your Supabase superuser URL) to get a scoped connection string.\n"+
				"  2. Open %s and replace db.connectionString with the value from step 1.\n"+
				"  3. Restart this MCP session.\n"+
				"  On Unix also: chmod 600 %s\n"+
				"  On Windows also: icacls \"%s\" /inheritance:r /grant:r \"%%USERNAME%%:F\"",
			e.Path, e.Path, e.Path, e.Path)
	}
	return fmt.Sprintf(
		"config at %s still has the placeholder db.connectionString. Fill it in (see `shared-memory-mcp-admin init`) and restart.",
		e.Path)
}

func Load() (*Config, error) {
	_, path := Paths()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if werr := writeTemplate(path); werr != nil {
			return nil, fmt.Errorf("bootstrap config: %w", werr)
		}
		return nil, &ErrNotConfigured{Path: path, Bootstrap: true}
	}

	c := &Config{}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := assertSecurePerms(path); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	envOverride(c)
	applyDefaults(c)

	if c.Device.ID == "" {
		return nil, fmt.Errorf("missing device.id in %s. Delete the file and restart to regenerate, or set SHARED_MEMORY_DEVICE_ID", path)
	}
	if c.DB.ConnectionString == ConnectionPlaceholder {
		return nil, &ErrNotConfigured{Path: path, Bootstrap: false}
	}
	// db.connectionString being empty is acceptable: local-only mode.
	return c, nil
}

// writeTemplate creates the config directory and writes a starter
// config.json with a fresh device.id and a placeholder connection
// string. File is created with 0600 mode (effective on Unix; on Windows
// inheriting parent ACL is typically sufficient).
func writeTemplate(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tpl := Config{
		DB: DBConfig{
			ConnectionString: ConnectionPlaceholder,
			CACertPath:       "",
		},
		Device:  DeviceConfig{ID: newDeviceID()},
		Project: ProjectConfig{},
		Sync:    SyncConfig{IntervalSeconds: 60, PageSize: 1000},
	}
	body, err := json.MarshalIndent(tpl, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o600)
}

func newDeviceID() string {
	var b [16]byte
	if _, err := cryptoRandRead(b[:]); err != nil {
		// Fallback: time-based string; not ideal but a non-empty id is
		// better than crashing, and the user can change it manually.
		return fmt.Sprintf("device-%d", os.Getpid())
	}
	// Format as RFC 4122 v4 UUID.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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
