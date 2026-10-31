package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Identity 是这台设备的身份：一对 Ed25519 密钥，公钥即设备 ID。
type Identity struct {
	ID   string // 公钥的 base64url
	priv ed25519.PrivateKey
}

func LoadIdentity(dir string) (*Identity, error) {
	path := filepath.Join(dir, "identity.key")
	var seed []byte
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := writeIdentity(path, seed); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		seed, err = hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("身份文件 %s 已损坏", path)
		}
	}
	return identityFromSeed(seed), nil
}

func identityFromSeed(seed []byte) *Identity {
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{ID: base64.RawURLEncoding.EncodeToString(pub), priv: priv}
}

func writeIdentity(path string, seed []byte) error {
	return os.WriteFile(path, []byte(hex.EncodeToString(seed)), 0o600)
}

// DeviceCode 是给人看的短设备码，例如 3F9A-C21B。
func DeviceCode(id string) string {
	sum := sha256.Sum256([]byte(id))
	h := strings.ToUpper(hex.EncodeToString(sum[:4]))
	return h[:4] + "-" + h[4:]
}

// AuthorTag 嵌在批注 ID 的前半段，用来在没有原始记录时也能判断"是不是作者本人"。
func AuthorTag(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

func RandomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func signedBytes(r *Record) ([]byte, error) {
	c := *r
	c.Sig = ""
	return json.Marshal(&c)
}

func (i *Identity) Sign(r *Record) error {
	b, err := signedBytes(r)
	if err != nil {
		return err
	}
	r.Sig = base64.RawURLEncoding.EncodeToString(ed25519.Sign(i.priv, b))
	return nil
}

func verifyRecord(r *Record) bool {
	pub, err := base64.RawURLEncoding.DecodeString(r.Author)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.Sig)
	if err != nil {
		return false
	}
	b, err := signedBytes(r)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), b, sig)
}
