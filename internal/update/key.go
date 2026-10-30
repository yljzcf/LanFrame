package update

import (
	"crypto/ed25519"
	"encoding/base64"
)

// publicKey 是验证更新清单用的 Ed25519 公钥（base64）。对应的私钥在 GitHub 仓库的 Secrets 里（UPDATE_SIGNING_KEY），
// 发布时由 tools/signupdate 签名。换了密钥，旧版本的程序就认不出新版本，大家只能手动更新一次。
// 写成变量是为了测试时能用 -ldflags "-X lanreview/internal/update.publicKey=..." 换成测试公钥。
var publicKey = "LavPvWpg19r2d/KXijNjW9Ng+JTDBk8RrT+wLqO20s4="

// SetPublicKey 换掉内置公钥，返回换回去的函数。只给其他包的测试用。
func SetPublicKey(pub ed25519.PublicKey) (restore func()) {
	old := publicKey
	publicKey = base64.StdEncoding.EncodeToString(pub)
	return func() { publicKey = old }
}
