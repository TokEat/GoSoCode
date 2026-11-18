package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration. Values come from environment
// variables so the same binary runs in dev and prod without code changes.
type Config struct {
	Addr        string // HTTP listen address, e.g. ":8080"
	DBPath      string // SQLite file path
	JWTSecret   string // secret for signing platform-user JWTs
	AppName     string // Baidu OpenAPI app name -> sandbox dir /apps/<AppName>/
	BaiduAppKey string // OAuth client id (AppKey)
	BaiduSecret string // OAuth client secret (SecretKey)
	RedirectURI string // OAuth redirect_uri (must match app config)
	RedisAddr   string // if set, use Redis-backed distributed lock + limiter
	Workers     int    // in-process worker count per node queue
	PerAccQPS   int    // max Baidu API QPS per node (rate limiter)

	StreamEnabled       bool // feature gate for pipelined (streaming) uploads
	StreamMaxConcurrent int  // max simultaneous streaming-upload sessions (memory guard)
	StreamWindow        int  // in-flight slice window advertised to clients

	AgentEnabled    bool   // allow CLI clients to transfer directly against Baidu
	AgentWindow     int    // in-flight slice window advertised to CLI clients (bigger: localhost leg is free)
	AgentControlKey string // 32-byte hex key encrypting CLI control-plane packets
	WebUIDir        string // if the dir exists, serve the built web UI from it
	ClientDir       string // if set, serves CLI client binaries at /api/client/download/<os>/<arch>
	KeysDir         string // local backup copies of per-node encryption keys (second store besides the DB)
	NotesDir        string // local image files for path notes (next to the DB by default)

	DailyReconcile bool   // run a full netdisk-to-DB reconciliation every day at 00:00
	Timezone       string // IANA timezone for daily schedule; empty = server local time
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// DevJWTSecret is the development-only placeholder used when GSC_JWT_SECRET is
// unset. It must never be used in production.
const DevJWTSecret = "dev-insecure-change-me"

// IsProduction reports whether this process should be treated as a production
// deployment. GSC_ENV=production|prod wins; GSC_ENV=development|dev|test forces
// non-production; otherwise GIN_MODE=release is treated as production.
func IsProduction() bool {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("GSC_ENV")))
	switch env {
	case "production", "prod":
		return true
	case "development", "dev", "test":
		return false
	}
	return strings.EqualFold(os.Getenv("GIN_MODE"), "release")
}

// JWTSecretInsecure reports whether the configured signing secret is empty or
// a well-known placeholder that must not be used in production.
func (c *Config) JWTSecretInsecure() bool {
	if c == nil {
		return true
	}
	s := strings.TrimSpace(c.JWTSecret)
	if s == "" || s == DevJWTSecret {
		return true
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "change-me") || lower == "changeme" || lower == "secret" || lower == "jwt-secret" {
		return true
	}
	return false
}

// Validate checks runtime safety of the loaded config. In production an
// insecure JWT secret is fatal; in development a loud warning is logged.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if !c.JWTSecretInsecure() {
		return nil
	}
	msg := "GSC_JWT_SECRET is unset or uses an insecure default; set a strong random secret"
	if IsProduction() {
		return fmt.Errorf("%s (refusing to start in production)", msg)
	}
	log.Printf("[config] WARNING: %s. This is only acceptable for local development.", msg)
	return nil
}

func Load() *Config {
	cfg := &Config{
		Addr:        env("GSC_ADDR", ":8080"),
		DBPath:      env("GSC_DB", "gosocode.db"),
		JWTSecret:   env("GSC_JWT_SECRET", DevJWTSecret),
		AppName:     env("GSC_APP_NAME", "gosocode"),
		BaiduAppKey: env("GSC_BAIDU_APPKEY", ""),
		BaiduSecret: env("GSC_BAIDU_SECRET", ""),
		RedirectURI: env("GSC_REDIRECT_URI", "http://localhost:8080/api/oauth/callback"),
		RedisAddr:   env("GSC_REDIS", ""),
		Workers:     4,
		PerAccQPS:   8,

		StreamEnabled:       env("GSC_STREAM_ENABLED", "1") != "0",
		StreamMaxConcurrent: envInt("GSC_STREAM_MAX_CONCURRENT", 8),
		// Default 3: peak RAM is about window × slice_size per session (SVIP
		// slices are 32MB, so 3×32MB×8 sessions ≈ 768MB). Raising to 4 would
		// push that near 1GB; leave the default conservative and override with
		// GSC_STREAM_WINDOW when the host has headroom.
		StreamWindow: envInt("GSC_STREAM_WINDOW", 3),

		AgentEnabled:    env("GSC_AGENT_ENABLED", "1") != "0",
		AgentWindow:     envInt("GSC_AGENT_WINDOW", 8),
		AgentControlKey: env("GSC_AGENT_CONTROL_KEY", ""),
		WebUIDir:        env("GSC_WEBUI", "webui"),
		ClientDir:       env("GSC_CLIENT_DIR", ""),
		KeysDir:         env("GSC_KEYS_DIR", filepath.Join(filepath.Dir(env("GSC_DB", "gosocode.db")), "gsc-keys")),
		NotesDir:        env("GSC_NOTES_DIR", filepath.Join(filepath.Dir(env("GSC_DB", "gosocode.db")), "gsc-notes")),

		DailyReconcile: env("GSC_DAILY_RECONCILE", "1") != "0",
		Timezone:       env("GSC_TIMEZONE", ""),
	}
	if cfg.Timezone != "" {
		if _, err := time.LoadLocation(cfg.Timezone); err != nil {
			log.Printf("[config] invalid GSC_TIMEZONE %q: %v; using local time", cfg.Timezone, err)
			cfg.Timezone = ""
		}
	}
	return cfg
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
