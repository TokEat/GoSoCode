package main

import (
	"flag"
	"log"
	"os"
	"time"

	"gosocode/internal/account"
	"gosocode/internal/api"
	"gosocode/internal/baidu"
	"gosocode/internal/config"
	"gosocode/internal/coord"
	"gosocode/internal/crypto"
	"gosocode/internal/job"
	"gosocode/internal/manifest"
	"gosocode/internal/metrics"
	"gosocode/internal/models"
	"gosocode/internal/realtime"
	"gosocode/internal/store"
)

func main() {
	clientDir := flag.String("client-dir", "", "client 二进制目录（布局 <os>-<arch>/client[.exe]）；默认取 GSC_CLIENT_DIR，其次 ./client-bin")
	flag.Parse()

	cfg := config.Load()
	if *clientDir != "" {
		cfg.ClientDir = *clientDir
	} else if cfg.ClientDir == "" {
		cfg.ClientDir = "client-bin"
	}
	if st, err := os.Stat(cfg.ClientDir); err != nil || !st.IsDir() {
		log.Printf("client 下载未启用（目录 %s 不存在）", cfg.ClientDir)
		cfg.ClientDir = ""
	} else {
		log.Printf("client 下载目录: %s", cfg.ClientDir)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}

	client := baidu.New(cfg.BaiduAppKey, cfg.BaiduSecret)
	agentControl, err := crypto.New(cfg.AgentControlKey)
	if err != nil {
		log.Fatalf("agent control encryption: %v", err)
	}

	// Baidu app credentials live in the DB (editable from the admin UI). Seed the
	// singleton row from env on first run, then load it into the client.
	var bc models.BaiduAppConfig
	if err := db.First(&bc, 1).Error; err != nil {
		bc = models.BaiduAppConfig{
			ID: 1, AppKey: cfg.BaiduAppKey, SecretKey: cfg.BaiduSecret,
			RedirectURI: cfg.RedirectURI, UpdatedAt: time.Now(),
		}
		db.Create(&bc)
	}
	client.SetCreds(bc.AppKey, bc.SecretKey)

	pool := account.NewPool(db, client)
	co, err := coord.New(cfg.RedisAddr, cfg.PerAccQPS)
	if err != nil {
		log.Fatalf("coord: %v", err)
	}
	if cfg.RedisAddr != "" {
		log.Printf("coordination: redis @ %s (multi-instance safe)", cfg.RedisAddr)
	} else {
		log.Printf("coordination: in-memory (single instance)")
	}
	hub := realtime.NewHub()
	meter := metrics.New()
	msync := manifest.NewSyncer(db, pool, client, co, hub)
	jobs := job.NewManager(db, pool, client, co, hub, meter, msync, cfg.Workers, cfg.KeysDir)
	if cfg.DailyReconcile {
		msync.StartDailyReconcile(cfg.Timezone)
	}

	// Live traffic feed: push the meter snapshot to admin WebSocket connections
	// every 2s so the monitoring page doesn't need to poll for realtime stats.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			hub.BroadcastAdmins(map[string]any{"type": "traffic", "live": meter.Snapshot()})
		}
	}()

	srv := api.NewServer(cfg, db, pool, client, co, jobs, hub, meter, agentControl, msync)
	log.Printf("gosocode backend listening on %s", cfg.Addr)
	if err := srv.Router().Run(cfg.Addr); err != nil {
		log.Fatalf("server: %v", err)
	}
}
