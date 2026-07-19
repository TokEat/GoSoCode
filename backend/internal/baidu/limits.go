package baidu

// TransferLimits is the per-tier transfer tuning derived from Baidu OpenAPI
// rules and community practice (BaiduPCS-Go, Alist/OpenList Baidu drivers):
//
//   - superfile2 slice size is a hard API cap per tier (4/16/32MB);
//   - dlink downloads: 8h validity, UA pan.baidu.com required; the official
//     docs recommend ~4 concurrent range connections to avoid failed
//     requests (soft guidance, not a hard cap). Per-connection speed is
//     heavily throttled for 普通, unthrottled for SVIP — SVIP setups run
//     8-16 connections stably in practice, and our segment retry +
//     reconciliation absorbs the occasional refused request;
//   - SVIP growth levels V1-V10 only change storage/file-size quotas, not
//     speed; the quota API reports vip_type 0/1/2 only, so all SVIP levels
//     share one profile;
//   - upload throughput is uplink-bound; the window caps bytes in flight so a
//     slow uplink doesn't trickle many huge slices in parallel (the historic
//     30s-per-slice server rule has been relaxed, but Baidu still drops
//     stalled-looking connections and a dropped slice is fully resent).
type TransferLimits struct {
	SliceSize     int64 // upload slice size (API cap per tier)
	UploadWindow  int   // max concurrent slice uploads (in flight = window × slice)
	DownloadConns int   // max parallel ranged connections per download
	DownloadSeg   int64 // ranged download segment per connection
}

// LimitsForVip returns the tuning for a vip_type (0 普通, 1 会员, 2+ 超级会员).
func LimitsForVip(vipType int) TransferLimits {
	switch {
	case vipType >= 2: // SVIP: unthrottled connections; big segments cut per-request overhead
		return TransferLimits{SliceSize: 32 << 20, UploadWindow: 4, DownloadConns: 10, DownloadSeg: 16 << 20}
	case vipType == 1: // VIP
		return TransferLimits{SliceSize: 16 << 20, UploadWindow: 6, DownloadConns: 8, DownloadSeg: 8 << 20}
	default: // 普通: heavily throttled per connection; small segments finish and retry fast
		return TransferLimits{SliceSize: ChunkSize, UploadWindow: 8, DownloadConns: 4, DownloadSeg: 4 << 20}
	}
}

// LimitsForSliceSize infers the tier limits from the slice size the server
// planned (the agent upload init does not carry vip_type).
func LimitsForSliceSize(sliceSize int64) TransferLimits {
	switch {
	case sliceSize >= 32<<20:
		return LimitsForVip(2)
	case sliceSize >= 16<<20:
		return LimitsForVip(1)
	default:
		return LimitsForVip(0)
	}
}

// TierName is the user-facing label for a vip_type.
func TierName(vipType int) string {
	switch {
	case vipType >= 2:
		return "超级会员"
	case vipType == 1:
		return "会员"
	default:
		return "普通账号"
	}
}
