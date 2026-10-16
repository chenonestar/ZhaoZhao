package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

// 加密的 docx/xlsx/pptx：默认密码（打开时不要密码）的解密后正常提取；真正的打开密码、权限管理分别说明原因。
// 样本由 tools/make_encrypted_ooxml.py 生成，并用 msoffcrypto 独立核对过。
func TestEncryptedOOXML(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "encrypted")
	for _, c := range []struct {
		name, ext string
		status    string
		want      string
	}{
		{"默认密码.xlsx", "xlsx", doc.StatusOK, "雪豹"},
		{"默认密码.docx", "docx", doc.StatusOK, "藏羚羊"},
		{"默认密码.pptx", "pptx", doc.StatusOK, "白鳍豚"},
		{"标准加密.xlsx", "xlsx", doc.StatusOK, "金丝猴"},
		{"打开密码.xlsx", "xlsx", doc.StatusEncrypted, "需要打开密码"},
		{"权限管理.xlsx", "xlsx", doc.StatusEncrypted, "权限管理"},
		// 扩展名不对也一样：模板、带宏的格式，以及改成 .xls 的
		{"默认密码.xlsx", "xltm", doc.StatusOK, "雪豹"},
		{"默认密码.xlsx", "xls", doc.StatusOK, "雪豹"},
	} {
		p := filepath.Join(dir, c.name)
		if c.ext != strings.TrimPrefix(filepath.Ext(c.name), ".") {
			q := filepath.Join(t.TempDir(), "x."+c.ext)
			b, _ := os.ReadFile(p)
			os.WriteFile(q, b, 0o644)
			p = q
		}
		res := Extract(p, c.ext, Options{})
		got := res.Text
		if res.Status != doc.StatusOK {
			got = res.Err
		}
		if res.Status != c.status || !strings.Contains(got, c.want) {
			t.Errorf("%s（.%s）：状态 %v，%q，应为 %v 且包含 %q", c.name, c.ext, res.Status, got, c.status, c.want)
		}
	}
}

// 解密的错误输入不能崩溃，只返回不支持或密码不对。
func TestOOXMLDecryptGarbage(t *testing.T) {
	for _, info := range [][]byte{nil, {4, 0, 4, 0, 0x40, 0, 0, 0, '<', 'x'}, {3, 0, 2, 0, 0x24, 0, 0, 0, 0xff, 0xff, 0, 0},
		{4, 0, 4, 0, 0x40, 0, 0, 0}} {
		if _, err := ooxmlDecrypt(info, make([]byte, 64), xlsDefaultPassword); err == nil {
			t.Fatalf("%v 应返回错误", info)
		}
	}
}

// 篡改过的 Agile 参数（哈希长度、盐长度、迭代次数）返回错误，不越界。
func TestOOXMLDecryptBadParams(t *testing.T) {
	s, _, err := oleStreams(filepath.Join("..", "..", "testdata", "encrypted", "默认密码.xlsx"), "EncryptionInfo", "EncryptedPackage")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]string{{`hashSize="64"`, `hashSize="200"`}, {`saltSize="16"`, `saltSize="-5"`}, {`saltSize="16"`, `saltSize="999"`},
		{`spinCount="100000"`, `spinCount="1"`}, {`keyBits="256"`, `keyBits="512"`}, {`blockSize="16"`, `blockSize="8"`}} {
		info := []byte(strings.Replace(string(s["EncryptionInfo"]), r[0], r[1], -1))
		if string(info) == string(s["EncryptionInfo"]) {
			t.Fatalf("样本里没有 %s", r[0])
		}
		if _, err := ooxmlDecrypt(info, s["EncryptedPackage"], xlsDefaultPassword); err == nil {
			t.Errorf("%s 应返回错误", r[1])
		}
	}
}

// 随便改动加密参数都不能崩溃。
func FuzzOOXMLDecrypt(f *testing.F) {
	for _, n := range []string{"默认密码.xlsx", "标准加密.xlsx"} {
		s, _, err := oleStreams(filepath.Join("..", "..", "testdata", "encrypted", n), "EncryptionInfo", "EncryptedPackage")
		if err != nil {
			f.Fatal(err)
		}
		f.Add(s["EncryptionInfo"], s["EncryptedPackage"][:min(len(s["EncryptedPackage"]), 8200)])
	}
	f.Fuzz(func(t *testing.T, info, pkg []byte) {
		ooxmlDecrypt(info, pkg, xlsDefaultPassword)
	})
}
