package push

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	fcmScope     = "https://www.googleapis.com/auth/firebase.messaging"
	defaultToken = "https://oauth2.googleapis.com/token"
)

func setting(ctx context.Context, db *sql.DB, key string) string {
	var value string
	_ = db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&value)
	return strings.TrimSpace(value)
}

func settingBool(ctx context.Context, db *sql.DB, key string, def bool) bool {
	v := strings.ToLower(setting(ctx, db, key))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return def
	}
}

func b64urlJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type fcmProvider struct {
	db     *sql.DB
	client *http.Client
	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewFCMProvider returns a Firebase Cloud Messaging HTTP v1 provider.
func NewFCMProvider(db *sql.DB) Provider {
	return &fcmProvider{db: db, client: &http.Client{Timeout: 20 * time.Second}}
}

type serviceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

func (p *fcmProvider) Send(ctx context.Context, device MobileDevice, msg Message) ProviderResult {
	if !settingBool(ctx, p.db, "mobile_push_enabled", true) || !settingBool(ctx, p.db, "fcm_enabled", false) {
		return ProviderResult{Terminal: false, Err: errors.New("FCM provider is disabled")}
	}
	accessToken, projectID, err := p.accessToken(ctx)
	if err != nil {
		return ProviderResult{Err: err}
	}
	if projectID == "" {
		return ProviderResult{Err: errors.New("fcm_project_id is not configured")}
	}

	data := map[string]string{}
	for k, v := range msg.Data {
		data[k] = v
	}
	data["title"] = msg.Title
	data["body"] = msg.Body
	data["severity"] = msg.Severity
	if msg.DeepLink != "" {
		data["deep_link"] = msg.DeepLink
	}

	body := map[string]any{
		"message": map[string]any{
			"token": device.Token,
			"notification": map[string]string{
				"title": msg.Title,
				"body":  msg.Body,
			},
			"data": data,
			"android": map[string]any{
				"priority":     "HIGH",
				"collapse_key": msg.Collapse,
				"notification": map[string]any{
					"channel_id":   channelForSeverity(msg.Severity),
					"tag":          msg.Collapse,
					"click_action": "OPEN_ALERT",
				},
			},
			"apns": map[string]any{
				"headers": map[string]string{
					"apns-push-type": "alert",
					"apns-priority":  "10",
				},
				"payload": apnsPayload(msg),
			},
		},
	}
	if msg.Collapse != "" {
		body["message"].(map[string]any)["apns"].(map[string]any)["headers"].(map[string]string)["apns-collapse-id"] = msg.Collapse
	}

	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", url.PathEscape(projectID)), bytes.NewReader(buf))
	if err != nil {
		return ProviderResult{Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return ProviderResult{Err: err}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(respBody, &out)
		return ProviderResult{ProviderMessageID: out.Name}
	}
	terminal := resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone
	return ProviderResult{Terminal: terminal, Err: fmt.Errorf("FCM returned %d: %s", resp.StatusCode, string(respBody))}
}

func (p *fcmProvider) accessToken(ctx context.Context) (string, string, error) {
	p.mu.Lock()
	if p.token != "" && time.Now().Before(p.expiry.Add(-5*time.Minute)) {
		projectID := setting(ctx, p.db, "fcm_project_id")
		p.mu.Unlock()
		return p.token, projectID, nil
	}
	p.mu.Unlock()

	var sa serviceAccount
	saJSON := setting(ctx, p.db, "fcm_service_account_json")
	if saJSON == "" {
		return "", "", errors.New("fcm_service_account_json is not configured")
	}
	if err := json.Unmarshal([]byte(saJSON), &sa); err != nil {
		return "", "", fmt.Errorf("invalid fcm_service_account_json: %w", err)
	}
	projectID := setting(ctx, p.db, "fcm_project_id")
	if projectID == "" {
		projectID = sa.ProjectID
	}
	if sa.TokenURI == "" {
		sa.TokenURI = defaultToken
	}
	jwt, err := makeRS256JWT(sa.ClientEmail, sa.TokenURI, fcmScope, sa.PrivateKey, time.Now())
	if err != nil {
		return "", projectID, err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", jwt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", projectID, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", projectID, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", projectID, fmt.Errorf("FCM OAuth returned %d: %s", resp.StatusCode, string(respBody))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return "", projectID, err
	}
	if tok.AccessToken == "" {
		return "", projectID, errors.New("FCM OAuth response missing access_token")
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	p.mu.Lock()
	p.token = tok.AccessToken
	p.expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	p.mu.Unlock()
	return tok.AccessToken, projectID, nil
}

func makeRS256JWT(clientEmail, tokenURI, scope, privateKeyPEM string, now time.Time) (string, error) {
	if clientEmail == "" || privateKeyPEM == "" {
		return "", errors.New("service account client_email/private_key missing")
	}
	header, err := b64urlJSON(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := b64urlJSON(map[string]any{
		"iss":   clientEmail,
		"scope": scope,
		"aud":   tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	input := header + "." + claims
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("invalid RSA private key PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

type apnsProvider struct {
	db     *sql.DB
	client *http.Client
	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewAPNSProvider returns an Apple Push Notification service provider.
func NewAPNSProvider(db *sql.DB) Provider {
	return &apnsProvider{db: db, client: &http.Client{Timeout: 20 * time.Second}}
}

func (p *apnsProvider) Send(ctx context.Context, device MobileDevice, msg Message) ProviderResult {
	if !settingBool(ctx, p.db, "mobile_push_enabled", true) || !settingBool(ctx, p.db, "apns_enabled", false) {
		return ProviderResult{Terminal: false, Err: errors.New("APNs provider is disabled")}
	}
	bundleID := setting(ctx, p.db, "apns_bundle_id")
	if bundleID == "" {
		return ProviderResult{Err: errors.New("apns_bundle_id is not configured")}
	}
	jwt, err := p.authJWT(ctx)
	if err != nil {
		return ProviderResult{Err: err}
	}
	host := "https://api.sandbox.push.apple.com"
	if settingBool(ctx, p.db, "apns_production", false) {
		host = "https://api.push.apple.com"
	}
	payload := apnsPayload(msg)
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/3/device/"+url.PathEscape(device.Token), bytes.NewReader(buf))
	if err != nil {
		return ProviderResult{Err: err}
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", bundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	if msg.Collapse != "" {
		req.Header.Set("apns-collapse-id", msg.Collapse)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ProviderResult{Err: err}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ProviderResult{ProviderMessageID: resp.Header.Get("apns-id")}
	}
	terminal := resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusGone
	return ProviderResult{Terminal: terminal, Err: fmt.Errorf("APNs returned %d: %s", resp.StatusCode, string(respBody))}
}

func (p *apnsProvider) authJWT(ctx context.Context) (string, error) {
	p.mu.Lock()
	if p.token != "" && time.Now().Before(p.expiry.Add(-5*time.Minute)) {
		defer p.mu.Unlock()
		return p.token, nil
	}
	p.mu.Unlock()

	teamID := setting(ctx, p.db, "apns_team_id")
	keyID := setting(ctx, p.db, "apns_key_id")
	privateKey := setting(ctx, p.db, "apns_private_key_p8")
	if teamID == "" || keyID == "" || privateKey == "" {
		return "", errors.New("apns_team_id, apns_key_id, and apns_private_key_p8 are required")
	}
	jwt, err := makeES256JWT(teamID, keyID, privateKey, time.Now())
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.token = jwt
	p.expiry = time.Now().Add(50 * time.Minute)
	p.mu.Unlock()
	return jwt, nil
}

func makeES256JWT(teamID, keyID, privateKeyPEM string, now time.Time) (string, error) {
	header, err := b64urlJSON(map[string]string{"alg": "ES256", "kid": keyID})
	if err != nil {
		return "", err
	}
	claims, err := b64urlJSON(map[string]any{"iss": teamID, "iat": now.Unix()})
	if err != nil {
		return "", err
	}
	input := header + "." + claims
	key, err := parseECPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseECPrivateKey(pemText string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("invalid EC private key PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if ecKey, ok := key.(*ecdsa.PrivateKey); ok && ecKey.Curve == elliptic.P256() {
			return ecKey, nil
		}
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func apnsPayload(msg Message) map[string]any {
	aps := map[string]any{
		"alert": map[string]string{
			"title": msg.Title,
			"body":  msg.Body,
		},
		"sound": "default",
	}
	if strings.EqualFold(msg.Severity, "critical") || settingSeverityTimeSensitive(msg.Severity) {
		aps["interruption-level"] = "time-sensitive"
	}
	payload := map[string]any{"aps": aps}
	for k, v := range msg.Data {
		payload[k] = v
	}
	if msg.DeepLink != "" {
		payload["deep_link"] = msg.DeepLink
	}
	return payload
}

func settingSeverityTimeSensitive(sev string) bool {
	return strings.EqualFold(sev, "critical")
}

func channelForSeverity(sev string) string {
	switch strings.ToLower(sev) {
	case "critical":
		return "down_hosts"
	case "warning":
		return "warnings"
	default:
		return "general"
	}
}
