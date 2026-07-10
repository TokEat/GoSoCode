package config

import (
	"os"
	"strconv"
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
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func Load() *Config {
	return &Config{
		Addr:        env("GSC_ADDR", ":8080"),
		DBPath:      env("GSC_DB", "gosocode.db"),
		JWTSecret:   env("GSC_JWT_SECRET", "dev-insecure-change-me"),
		AppName:     env("GSC_APP_NAME", "gosocode"),
		BaiduAppKey: env("GSC_BAIDU_APPKEY", ""),
		BaiduSecret: env("GSC_BAIDU_SECRET", ""),
		RedirectURI: env("GSC_REDIRECT_URI", "http://localhost:8080/api/oauth/callback"),
		RedisAddr:   env("GSC_REDIS", ""),
		Workers:     4,
		PerAccQPS:   8,

		StreamEnabled:       env("GSC_STREAM_ENABLED", "1") != "0",
		StreamMaxConcurrent: envInt("GSC_STREAM_MAX_CONCURRENT", 8),
		StreamWindow:        envInt("GSC_STREAM_WINDOW", 3),
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
