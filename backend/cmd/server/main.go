package main

import (
	"log"
	"time"

	"gosocode/internal/account"
	"gosocode/internal/api"
	"gosocode/internal/baidu"
	"gosocode/internal/config"
	"gosocode/internal/coord"
	"gosocode/internal/job"
	"gosocode/internal/metrics"
	"gosocode/internal/models"
	"gosocode/internal/realtime"
	"gosocode/internal/store"
)

func main() {
	cfg := config.Load()

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}

	client := baidu.New(cfg.BaiduAppKey, cfg.BaiduSecret)

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
	jobs := job.NewManager(db, pool, client, co, hub, meter, cfg.Workers)

	// Live traffic feed: push the meter snapshot to admin WebSocket connections
	// every 2s so the monitoring page doesn't need to poll for realtime stats.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			hub.BroadcastAdmins(map[string]any{"type": "traffic", "live": meter.Snapshot()})
		}
	}()

	srv := api.NewServer(cfg, db, pool, client, co, jobs, hub, meter)
	log.Printf("gosocode backend listening on %s", cfg.Addr)
	if err := srv.Router().Run(cfg.Addr); err != nil {
		log.Fatalf("server: %v", err)
	}
}
