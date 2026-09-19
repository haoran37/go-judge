package identity

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestReloadRejectsMismatchedPrivatePublicKey 覆盖 R7：损坏/公私混配的身份文件必须拒绝加载。
func TestReloadRejectsMismatchedPrivatePublicKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	if _, err := Open(path, "n", "formal"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st fileState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	// 用另一个合法公钥替换，私钥不变 => 对应关系被破坏。
	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(i + 1)
	}
	st.Keys[0].PublicKey = base64.StdEncoding.EncodeToString(other)
	tampered, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "n", "formal"); err == nil {
		t.Fatal("identity with mismatched private/public key loaded")
	}
}
