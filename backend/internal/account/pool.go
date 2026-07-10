// Package account manages the pool of shared Baidu accounts: load-balanced
// selection plus centralized access-token refresh (deduplicated so concurrent
// callers never trigger overlapping refreshes).
package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"

	"gosocode/internal/baidu"
	"gosocode/internal/models"
)

var ErrNoAccount = errors.New("no active baidu account available")

type Pool struct {
	db     *gorm.DB
	client *baidu.Client
	sf     singleflight.Group // dedupes token refresh per account id
}

func NewPool(db *gorm.DB, client *baidu.Client) *Pool {
	return &Pool{db: db, client: client}
}

// PickForSize implements capacity-aware placement for the distributed file
// system: among healthy storage nodes (Baidu accounts) with enough free space
// for the incoming file, it selects the least-loaded one, tie-breaking by most
// free space so data spreads across nodes. Callers must Release when done.
func (p *Pool) PickForSize(ctx context.Context, size int64) (*models.BaiduAccount, error) {
	var acc models.BaiduAccount
	// quota_total = 0 means quota unknown yet (not refreshed) -> still eligible.
	err := p.db.WithContext(ctx).
		Where("status = ?", "active").
		Where("quota_total = 0 OR quota_total - quota_used >= ?", size).
		Order("current_load asc").
		Order("quota_total - quota_used desc").
		First(&acc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNoAccount
	}
	if err != nil {
		return nil, err
	}
	p.db.Model(&acc).UpdateColumn("current_load", gorm.Expr("current_load + 1"))
	return &acc, nil
}

// PeekForSize previews which node PickForSize would choose without taking a
// load slot. Used by the streaming-upload "plan" step to tell the client the
// slice size before it computes the block md5 list.
func (p *Pool) PeekForSize(ctx context.Context, size int64) (*models.BaiduAccount, error) {
	var acc models.BaiduAccount
	err := p.db.WithContext(ctx).
		Where("status = ?", "active").
		Where("quota_total = 0 OR quota_total - quota_used >= ?", size).
		Order("current_load asc").
		Order("quota_total - quota_used desc").
		First(&acc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNoAccount
	}
	if err != nil {
		return nil, err
	}
	return &acc, nil
}

func (p *Pool) PickByIDForSize(ctx context.Context, id uint, size int64) (*models.BaiduAccount, error) {
	var acc models.BaiduAccount
	err := p.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, "active").
		Where("quota_total = 0 OR quota_total - quota_used >= ?", size).
		First(&acc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNoAccount
	}
	if err != nil {
		return nil, err
	}
	p.db.Model(&acc).UpdateColumn("current_load", gorm.Expr("current_load + 1"))
	return &acc, nil
}

// AddUsed reflects a completed upload in the node's used-quota accounting so
// subsequent placement decisions see the space consumed.
func (p *Pool) AddUsed(id uint, delta int64) {
	p.db.Model(&models.BaiduAccount{}).Where("id = ?", id).
		UpdateColumn("quota_used", gorm.Expr("quota_used + ?", delta))
}

// RefreshQuota queries the node's real capacity from Baidu and stores it.
func (p *Pool) RefreshQuota(ctx context.Context, acc *models.BaiduAccount) error {
	token, err := p.Token(ctx, acc)
	if err != nil {
		return err
	}
	q, err := p.client.GetQuota(ctx, token)
	if err != nil {
		return err
	}
	updates := map[string]any{"quota_total": q.Total, "quota_used": q.Used}
	// Best-effort VIP tier refresh: it decides the upload slice size (4/16/32MB).
	if ui, uerr := p.client.GetUserInfo(ctx, token); uerr == nil {
		updates["vip_type"] = ui.VipType
		if name := strings.TrimSpace(ui.NetdiskName); name != "" {
			if acc.Nickname == "" || acc.Nickname == "node" || strings.HasPrefix(acc.Nickname, "node-") {
				updates["nickname"] = name
			}
		} else if name := strings.TrimSpace(ui.BaiduName); name != "" {
			if acc.Nickname == "" || acc.Nickname == "node" || strings.HasPrefix(acc.Nickname, "node-") {
				updates["nickname"] = name
			}
		}
	}
	return p.db.Model(&models.BaiduAccount{}).Where("id = ?", acc.ID).
		Updates(updates).Error
}

// Get loads a specific account (e.g. the one that holds a file being downloaded).
func (p *Pool) Get(ctx context.Context, id uint) (*models.BaiduAccount, error) {
	var acc models.BaiduAccount
	if err := p.db.WithContext(ctx).First(&acc, id).Error; err != nil {
		return nil, err
	}
	return &acc, nil
}

func (p *Pool) Release(id uint) {
	p.db.Model(&models.BaiduAccount{}).Where("id = ?", id).
		UpdateColumn("current_load", gorm.Expr("current_load - 1"))
}

// Token returns a valid access token for the account, refreshing it if it
// expires within the next 3 days. singleflight guarantees a single in-flight
// refresh per account even under heavy concurrency.
func (p *Pool) Token(ctx context.Context, acc *models.BaiduAccount) (string, error) {
	if time.Until(acc.ExpiresAt) > 72*time.Hour && acc.AccessToken != "" {
		return acc.AccessToken, nil
	}
	key := "refresh:" + itoa(acc.ID)
	v, err, _ := p.sf.Do(key, func() (any, error) {
		// re-read to avoid refreshing on stale state
		var cur models.BaiduAccount
		if err := p.db.WithContext(ctx).First(&cur, acc.ID).Error; err != nil {
			return "", err
		}
		if time.Until(cur.ExpiresAt) > 72*time.Hour && cur.AccessToken != "" {
			return cur.AccessToken, nil
		}
		tr, err := p.client.RefreshToken(ctx, cur.RefreshToken)
		if err != nil {
			return "", err
		}
		cur.AccessToken = tr.AccessToken
		if tr.RefreshToken != "" {
			cur.RefreshToken = tr.RefreshToken
		}
		cur.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
		if err := p.db.WithContext(ctx).Save(&cur).Error; err != nil {
			return "", err
		}
		return cur.AccessToken, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func itoa(u uint) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
