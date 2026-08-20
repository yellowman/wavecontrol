package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"database/sql"
)

const (
	prefix      = "enc:v1:"
	MaskedValue = "********"
)

// Manager encrypts operational credentials with AES-256-GCM. The data key is
// deliberately separate from the JWT signing key so rotating sessions does not
// make stored credentials unreadable.
type Manager struct {
	aead cipher.AEAD
}

// New parses a 32-byte key encoded as base64 (standard or raw) or hex.
func New(encodedKey string) (*Manager, error) {
	key, err := decodeKey(strings.TrimSpace(encodedKey))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return &Manager{aead: aead}, nil
}

func decodeKey(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("WAVECONTROL_DATA_KEY is required")
	}
	decoders := []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range decoders {
		if b, err := decode(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("WAVECONTROL_DATA_KEY must encode exactly 32 bytes (base64 or hex)")
}

func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, prefix)
}

func (m *Manager) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	// Encrypt always treats its argument as plaintext. This is important for a
	// legitimate password whose first bytes happen to be "enc:v1:". Idempotence
	// for database migration is handled separately by encryptForMigration.
	nonce := make([]byte, m.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := m.aead.Seal(nil, nonce, []byte(plaintext), nil)
	buf := append(nonce, sealed...)
	return prefix + base64.RawStdEncoding.EncodeToString(buf), nil
}

// Decrypt accepts legacy plaintext so a database can be migrated atomically at
// startup. All new writes should be encrypted before reaching the database.
func (m *Manager) Decrypt(value string) (string, error) {
	if value == "" || !IsEncrypted(value) {
		return value, nil
	}
	buf, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted value: %w", err)
	}
	if len(buf) < m.aead.NonceSize() {
		return "", errors.New("encrypted value is truncated")
	}
	nonce, ciphertext := buf[:m.aead.NonceSize()], buf[m.aead.NonceSize():]
	plain, err := m.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt value: %w", err)
	}
	return string(plain), nil
}

// encryptForMigration leaves authenticated ciphertext unchanged, encrypts
// legacy plaintext, and fails closed when a prefixed value cannot be opened
// with the configured data key. Treating malformed or wrong-key ciphertext as
// plaintext would permanently conceal the key mismatch and corrupt access.
func (m *Manager) encryptForMigration(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if IsEncrypted(value) {
		if _, err := m.Decrypt(value); err != nil {
			return "", err
		}
		return value, nil
	}
	return m.Encrypt(value)
}

var secretSettingKeys = map[string]struct{}{
	"ap_cred1_pass": {}, "ap_cred2_pass": {}, "ap_cred3_pass": {},
	"sta_cred1_pass": {}, "sta_cred2_pass": {}, "sta_cred3_pass": {},
	"ap_passwords": {}, "sta_passwords": {}, "default_passwords": {},
	"default_password": {}, "default_sta_password": {}, "smtp_password": {},
}

func IsSecretSetting(key string) bool {
	_, ok := secretSettingKeys[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

// MigrateDatabase encrypts legacy plaintext operational secrets in a single
// transaction. It is idempotent and leaves already encrypted values untouched.
func (m *Manager) MigrateDatabase(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Ciphertext is longer than the legacy VARCHAR(128) password column.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE devices ALTER COLUMN password TYPE TEXT`); err != nil {
		return fmt.Errorf("widen devices.password: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(password, '') FROM devices WHERE COALESCE(password, '') <> '' FOR UPDATE`)
	if err != nil {
		return fmt.Errorf("read device passwords: %w", err)
	}
	type deviceSecret struct {
		id    int64
		value string
	}
	var devices []deviceSecret
	for rows.Next() {
		var d deviceSecret
		if err := rows.Scan(&d.id, &d.value); err != nil {
			rows.Close()
			return err
		}
		devices = append(devices, d)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, d := range devices {
		enc, err := m.encryptForMigration(d.value)
		if err != nil {
			return err
		}
		if enc == d.value {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET password = $1 WHERE id = $2`, enc, d.id); err != nil {
			return fmt.Errorf("encrypt password for device %d: %w", d.id, err)
		}
	}

	// Drilldown hosts can carry an explicit credential override. Encrypt it with
	// the same key and widen the legacy column before writing ciphertext.
	var hasDrilldownHosts bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('public.drilldown_hosts') IS NOT NULL`).Scan(&hasDrilldownHosts); err != nil {
		return err
	}
	if hasDrilldownHosts {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE drilldown_hosts ALTER COLUMN password TYPE TEXT`); err != nil {
			return fmt.Errorf("widen drilldown_hosts.password: %w", err)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(password, '') FROM drilldown_hosts WHERE COALESCE(password, '') <> '' FOR UPDATE`)
		if err != nil {
			return fmt.Errorf("read drilldown passwords: %w", err)
		}
		var hosts []deviceSecret
		for rows.Next() {
			var d deviceSecret
			if err := rows.Scan(&d.id, &d.value); err != nil {
				rows.Close()
				return err
			}
			hosts = append(hosts, d)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, d := range hosts {
			enc, err := m.encryptForMigration(d.value)
			if err != nil {
				return err
			}
			if enc == d.value {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE drilldown_hosts SET password=$1 WHERE id=$2`, enc, d.id); err != nil {
				return fmt.Errorf("encrypt drilldown password %d: %w", d.id, err)
			}
		}
	}

	settingRows, err := tx.QueryContext(ctx, `SELECT key, value FROM settings FOR UPDATE`)
	if err != nil {
		return fmt.Errorf("read settings secrets: %w", err)
	}
	type settingSecret struct{ key, value string }
	var settings []settingSecret
	for settingRows.Next() {
		var s settingSecret
		if err := settingRows.Scan(&s.key, &s.value); err != nil {
			settingRows.Close()
			return err
		}
		if IsSecretSetting(s.key) && s.value != "" {
			settings = append(settings, s)
		}
	}
	if err := settingRows.Close(); err != nil {
		return err
	}
	for _, s := range settings {
		enc, err := m.encryptForMigration(s.value)
		if err != nil {
			return fmt.Errorf("validate or encrypt setting %s: %w", s.key, err)
		}
		if enc != s.value {
			if _, err := tx.ExecContext(ctx, `UPDATE settings SET value = $1, updated_at = NOW() WHERE key = $2`, enc, s.key); err != nil {
				return fmt.Errorf("encrypt setting %s: %w", s.key, err)
			}
		}
	}

	// Older installations may contain a device_credentials table. Migrate it
	// when present without making it a required part of the current schema.
	var hasCredTable bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('public.device_credentials') IS NOT NULL`).Scan(&hasCredTable); err != nil {
		return err
	}
	if hasCredTable {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE device_credentials ALTER COLUMN password TYPE TEXT`); err != nil {
			return fmt.Errorf("widen device_credentials.password: %w", err)
		}
		credRows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(password, '') FROM device_credentials WHERE COALESCE(password, '') <> '' FOR UPDATE`)
		if err != nil {
			return err
		}
		var creds []deviceSecret
		for credRows.Next() {
			var d deviceSecret
			if err := credRows.Scan(&d.id, &d.value); err != nil {
				credRows.Close()
				return err
			}
			creds = append(creds, d)
		}
		if err := credRows.Close(); err != nil {
			return err
		}
		for _, d := range creds {
			enc, err := m.encryptForMigration(d.value)
			if err != nil {
				return err
			}
			if enc == d.value {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE device_credentials SET password = $1 WHERE id = $2`, enc, d.id); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}
