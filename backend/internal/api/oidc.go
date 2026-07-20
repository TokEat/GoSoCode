// Unified OIDC (Keycloak-compatible) single sign-on.
//
// Standard authorization-code flow against any OIDC provider that serves a
// discovery document (Keycloak realm issuer: https://<host>/realms/<realm>).
// The provider only authenticates; authorization stays local: the callback
// maps the OIDC subject to a local User (auto-provisioned as role=user on
// first login) and issues the platform's own JWT, so authMiddleware and the
// rest of the API are unchanged.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gosocode/internal/models"
)

type oidcEndpoints struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

var oidcHTTP = &http.Client{Timeout: 10 * time.Second}

// oidcDiscover fetches (with a 10-minute cache) the issuer's discovery
// document. The cache is keyed by issuer so an admin changing the setting
// takes effect immediately.
func (s *Server) oidcDiscover(ctx context.Context, issuer string) (*oidcEndpoints, error) {
	issuer = strings.TrimRight(issuer, "/")
	s.oidcMu.Lock()
	if s.oidcCache != nil && s.oidcCacheIssuer == issuer && time.Since(s.oidcCacheAt) < 10*time.Minute {
		ep := s.oidcCache
		s.oidcMu.Unlock()
		return ep, nil
	}
	s.oidcMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := oidcHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取 OIDC 发现文档失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC 发现文档返回 %d（请检查 Issuer 地址）", resp.StatusCode)
	}
	var ep oidcEndpoints
	if err := json.NewDecoder(resp.Body).Decode(&ep); err != nil {
		return nil, fmt.Errorf("解析 OIDC 发现文档失败: %w", err)
	}
	if ep.AuthorizationEndpoint == "" || ep.TokenEndpoint == "" || ep.UserinfoEndpoint == "" {
		return nil, fmt.Errorf("OIDC 发现文档缺少必要端点")
	}
	s.oidcMu.Lock()
	s.oidcCache, s.oidcCacheIssuer, s.oidcCacheAt = &ep, issuer, time.Now()
	s.oidcMu.Unlock()
	return &ep, nil
}

// oidcRedirectURI must be identical in the authorize redirect and the code
// exchange, and whitelisted in the Keycloak client. Admins can pin it via
// settings; otherwise it derives from the incoming request.
func (s *Server) oidcRedirectURI(c *gin.Context, sc models.SystemConfig) string {
	if sc.OIDCRedirectURL != "" {
		return sc.OIDCRedirectURL
	}
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host + "/api/auth/oidc/callback"
}

// handleOIDCStatus tells the (pre-auth) login page whether SSO is on.
func (s *Server) handleOIDCStatus(c *gin.Context) {
	sc := s.systemConfig()
	c.JSON(http.StatusOK, gin.H{"enabled": sc.OIDCEnabled && sc.OIDCIssuer != "" && sc.OIDCClientID != ""})
}

func (s *Server) handleOIDCLogin(c *gin.Context) {
	sc := s.systemConfig()
	if !sc.OIDCEnabled || sc.OIDCIssuer == "" || sc.OIDCClientID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "统一认证未启用"})
		return
	}
	ep, err := s.oidcDiscover(c.Request.Context(), sc.OIDCIssuer)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", sc.OIDCClientID)
	q.Set("redirect_uri", s.oidcRedirectURI(c, sc))
	q.Set("scope", "openid profile email")
	q.Set("state", s.mintOAuthState())
	sep := "?"
	if strings.Contains(ep.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	c.Redirect(http.StatusFound, ep.AuthorizationEndpoint+sep+q.Encode())
}

// oidcFail sends the browser back to the login page with a visible reason.
func oidcFail(c *gin.Context, msg string) {
	c.Redirect(http.StatusFound, "/#oidc_error="+url.QueryEscape(msg))
}

func (s *Server) handleOIDCCallback(c *gin.Context) {
	if !s.consumeOAuthState(c.Query("state")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired state"})
		return
	}
	if e := c.Query("error"); e != "" {
		oidcFail(c, "统一认证失败: "+e)
		return
	}
	code := c.Query("code")
	if code == "" {
		oidcFail(c, "统一认证失败: 未返回授权码")
		return
	}
	sc := s.systemConfig()
	if !sc.OIDCEnabled {
		oidcFail(c, "统一认证未启用")
		return
	}
	ctx := c.Request.Context()
	ep, err := s.oidcDiscover(ctx, sc.OIDCIssuer)
	if err != nil {
		oidcFail(c, err.Error())
		return
	}

	// code -> tokens (client_secret_post; accepted by Keycloak confidential clients)
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", s.oidcRedirectURI(c, sc))
	form.Set("client_id", sc.OIDCClientID)
	form.Set("client_secret", sc.OIDCClientSecret)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ep.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oidcHTTP.Do(req)
	if err != nil {
		oidcFail(c, "换取令牌失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		msg := tok.ErrorDescription
		if msg == "" {
			msg = tok.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("令牌端点返回 %d", resp.StatusCode)
		}
		oidcFail(c, "换取令牌失败: "+msg)
		return
	}

	// userinfo: authoritative identity claims, validated server-side by the provider
	uiReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ep.UserinfoEndpoint, nil)
	uiReq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	uiResp, err := oidcHTTP.Do(uiReq)
	if err != nil {
		oidcFail(c, "获取用户信息失败: "+err.Error())
		return
	}
	defer uiResp.Body.Close()
	var claims struct {
		Sub               string `json:"sub"`
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
		Name              string `json:"name"`
	}
	if err := json.NewDecoder(uiResp.Body).Decode(&claims); err != nil || claims.Sub == "" {
		oidcFail(c, "获取用户信息失败: 无有效 sub 声明")
		return
	}

	// Accounts are linked strictly by subject — never by username — so a
	// Keycloak user named like an existing local account (e.g. "admin")
	// cannot take it over.
	var u models.User
	if err := s.db.Where("oidc_subject = ?", claims.Sub).First(&u).Error; err != nil {
		base := claims.PreferredUsername
		if base == "" && claims.Email != "" {
			base = strings.SplitN(claims.Email, "@", 2)[0]
		}
		u = models.User{
			Username: s.uniqueUsername(sanitizeUsername(base, claims.Sub)),
			Role:     "user", Status: "active", OIDCSubject: claims.Sub,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := s.db.Create(&u).Error; err != nil {
			oidcFail(c, "创建用户失败: "+err.Error())
			return
		}
		s.audit(c, u.ID, u.Username, "user.provision.oidc", "sub="+claims.Sub)
	}
	if u.Status == "disabled" {
		oidcFail(c, "账号已被禁用，请联系管理员")
		return
	}
	s.audit(c, u.ID, u.Username, "login.oidc", "sub="+claims.Sub)
	c.Redirect(http.StatusFound,
		"/#oidc_token="+url.QueryEscape(s.issueToken(u.ID))+"&role="+url.QueryEscape(u.Role))
}

var usernameJunk = regexp.MustCompile(`[^0-9A-Za-z\p{Han}._-]+`)

func sanitizeUsername(base, sub string) string {
	base = usernameJunk.ReplaceAllString(strings.TrimSpace(base), "")
	if len(base) > 60 {
		base = base[:60]
	}
	if len(base) < 3 {
		if len(sub) > 8 {
			sub = sub[:8]
		}
		base = "sso-" + sub
	}
	return base
}

func (s *Server) uniqueUsername(base string) string {
	name := base
	for i := 2; ; i++ {
		var n int64
		s.db.Model(&models.User{}).Where("username = ?", name).Count(&n)
		if n == 0 {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}
