package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// clientPlatforms is the canonical list of (os, arch) targets the project
// ships. Keep in sync with .github/workflows/release.yml.
var clientPlatforms = []struct{ OS, Arch, Bin string }{
	{"linux", "amd64", "client"},
	{"linux", "arm64", "client"},
	{"windows", "amd64", "client.exe"},
	{"windows", "arm64", "client.exe"},
	{"darwin", "amd64", "client"},
	{"darwin", "arm64", "client"},
}

// clientDownloadInfo is the JSON shape returned by /api/client/download/info.
type clientDownloadInfo struct {
	Enabled   bool                `json:"enabled"`
	Platforms []clientDownloadRow `json:"platforms"`
	Detected  *detectedPlatform   `json:"detected,omitempty"`
}

type detectedPlatform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// detectPlatformFromUA maps the browser User-Agent to one of the shipped
// (os, arch) targets. Arch defaults to amd64: browser UAs rarely expose it
// (e.g. Apple Silicon reports Intel). Mobile OSes return nil (no CLI client).
func detectPlatformFromUA(ua string) *detectedPlatform {
	ua = strings.ToLower(ua)
	var osName string
	switch {
	// iOS/Android UAs also contain "mac os"/"linux" — check them first.
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "android"):
		return nil
	case strings.Contains(ua, "windows"):
		osName = "windows"
	case strings.Contains(ua, "mac os"), strings.Contains(ua, "macintosh"), strings.Contains(ua, "darwin"):
		osName = "darwin"
	case strings.Contains(ua, "linux"), strings.Contains(ua, "x11"):
		osName = "linux"
	default:
		return nil
	}
	arch := "amd64"
	if strings.Contains(ua, "arm64") || strings.Contains(ua, "aarch64") {
		arch = "arm64"
	}
	return &detectedPlatform{OS: osName, Arch: arch}
}

type clientDownloadRow struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Size    int64  `json:"size"`
	Missing bool   `json:"missing,omitempty"`
}

// handleClientDownloadInfo reports which client binaries are present on disk.
// Used by the web UI to render the platform picker.
func (s *Server) handleClientDownloadInfo(c *gin.Context) {
	if s.cfg.ClientDir == "" {
		c.JSON(http.StatusOK, clientDownloadInfo{Enabled: false})
		return
	}
	rows := make([]clientDownloadRow, 0, len(clientPlatforms))
	for _, p := range clientPlatforms {
		path := filepath.Join(s.cfg.ClientDir, p.OS+"-"+p.Arch, p.Bin)
		row := clientDownloadRow{OS: p.OS, Arch: p.Arch}
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			row.Size = st.Size()
		} else {
			row.Missing = true
		}
		rows = append(rows, row)
	}
	c.JSON(http.StatusOK, clientDownloadInfo{
		Enabled:   true,
		Platforms: rows,
		Detected:  detectPlatformFromUA(c.GetHeader("User-Agent")),
	})
}

// handleClientDownload serves the CLI client binary for the requested
// platform. Path-traversal-safe: only known (os, arch) pairs from
// clientPlatforms are accepted; any other request 404s.
func (s *Server) handleClientDownload(c *gin.Context) {
	if s.cfg.ClientDir == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "client downloads disabled"})
		return
	}
	osName := strings.ToLower(c.Param("os"))
	arch := strings.ToLower(c.Param("arch"))

	var bin string
	matched := false
	for _, p := range clientPlatforms {
		if p.OS == osName && p.Arch == arch {
			bin = p.Bin
			matched = true
			break
		}
	}
	if !matched {
		c.JSON(http.StatusNotFound, gin.H{"error": "unsupported platform"})
		return
	}

	path := filepath.Join(s.cfg.ClientDir, osName+"-"+arch, bin)
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		c.JSON(http.StatusNotFound, gin.H{"error": "binary not found on server"})
		return
	}

	// filename for the browser's Save dialog: gosocode-client-<os>-<arch>[.exe]
	downloadName := "gosocode-client-" + osName + "-" + arch
	if strings.HasSuffix(bin, ".exe") {
		downloadName += ".exe"
	}
	c.Header("Content-Disposition", `attachment; filename="`+downloadName+`"`)
	c.Header("Content-Length", strconv.FormatInt(st.Size(), 10))
	c.File(path)
}
