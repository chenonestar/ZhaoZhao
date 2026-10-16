package extract

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"hash"
	"os"
	"strings"

	"github.com/chenonestar/zhaozhao/internal/doc"
	"github.com/richardlehane/mscfb"
)

// Office 2007 以后的 docx/xlsx/pptx 的加密（ECMA-376，MS-OFFCRYPTO 2.3.4）。
//
// 加密后的文件是 OLE 复合文档：EncryptionInfo 流是加密参数，EncryptedPackage 流是加密后的原 zip 包。
// 打开时不要密码的加密文件用的是 Excel 的默认密码 VelvetSweatshop（与 .xls 的「保护工作簿」相同），
// 先用它解密；解不开的才是真正设了打开密码的文件。
// 支持 Excel 2007 的标准加密（AES-ECB）和 2010 以后默认的 Agile 加密（AES-CBC，SHA-1/SHA-2）。

var (
	errOOXMLWrongPassword = errors.New("需要打开密码")
	errOOXMLUnsupported   = errors.New("不支持的加密方式")
)

// 加密文件的失败原因：区分需要打开密码、权限管理、不支持的加密方式，便于用户判断。
const (
	msgNeedPassword = "文件已加密（需要打开密码）"
	msgIRM          = "文件受权限管理保护（IRM / 敏感度标签），没有授权无法读取"
)

// ooxmlDecrypt 用密码解密 EncryptedPackage，返回原来的 zip 包。
func ooxmlDecrypt(info, pkg []byte, password string) ([]byte, error) {
	if len(info) < 8 || len(pkg) < 8 {
		return nil, errOOXMLUnsupported
	}
	major, minor := le16(info, 0), le16(info, 2)
	switch {
	case major == 4 && minor == 4:
		return agileDecrypt(info[8:], pkg, password)
	case (major == 2 || major == 3 || major == 4) && minor == 2:
		return standardDecrypt(info, pkg, password)
	}
	return nil, errOOXMLUnsupported // 3.3/4.3 是「可扩展加密」，Office 没有用过
}

// ---- 标准加密（MS-OFFCRYPTO 2.3.4.5～2.3.4.9）----

func standardDecrypt(info, pkg []byte, password string) ([]byte, error) {
	hsize := le32(info, 8)
	hdr := 12
	if hsize < 32 || hdr+hsize+4 > len(info) {
		return nil, errOOXMLUnsupported
	}
	algID, keyBits := le32(info, hdr+8), le32(info, hdr+16)
	if algID != 0x660E && algID != 0x660F && algID != 0x6610 { // AES-128/192/256；标准加密不用 RC4
		return nil, errOOXMLUnsupported
	}
	if keyBits == 0 {
		keyBits = 128
	}
	if keyBits != 128 && keyBits != 192 && keyBits != 256 {
		return nil, errOOXMLUnsupported
	}
	v := hdr + hsize
	if le32(info, v) != 16 || v+4+16+16+4+32 > len(info) {
		return nil, errOOXMLUnsupported
	}
	salt := info[v+4 : v+20]
	encVerifier := info[v+20 : v+36]
	encVerifierHash := info[v+40 : v+72]

	h := sha1.Sum(append(append([]byte{}, salt...), utf16leBytes(password)...))
	var buf [24]byte
	for i := 0; i < 50000; i++ {
		binary.LittleEndian.PutUint32(buf[:4], uint32(i))
		copy(buf[4:], h[:])
		h = sha1.Sum(buf[:])
	}
	copy(buf[:20], h[:])
	binary.LittleEndian.PutUint32(buf[20:], 0) // 第 0 块
	final := sha1.Sum(buf[:])
	derive := func(c byte) [20]byte {
		var b [64]byte
		for i := range b {
			b[i] = c
		}
		for i, x := range final {
			b[i] ^= x
		}
		return sha1.Sum(b[:])
	}
	x1, x2 := derive(0x36), derive(0x5c)
	key := append(x1[:], x2[:]...)[:keyBits/8]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errOOXMLUnsupported
	}
	verifier := ecbDecrypt(block, encVerifier)
	vh := ecbDecrypt(block, encVerifierHash)
	if want := sha1.Sum(verifier); !bytes.Equal(vh[:20], want[:]) {
		return nil, errOOXMLWrongPassword
	}
	size := binary.LittleEndian.Uint64(pkg)
	data := pkg[8:]
	data = data[:len(data)/aes.BlockSize*aes.BlockSize]
	out := ecbDecrypt(block, data)
	if size > uint64(len(out)) {
		return nil, errOOXMLUnsupported
	}
	return out[:size], nil
}

func ecbDecrypt(b cipher.Block, src []byte) []byte {
	out := make([]byte, len(src)/b.BlockSize()*b.BlockSize())
	for i := 0; i < len(out); i += b.BlockSize() {
		b.Decrypt(out[i:], src[i:])
	}
	return out
}

// ---- Agile 加密（MS-OFFCRYPTO 2.3.4.10～2.3.4.15）----

type agileParams struct {
	SaltSize        int    `xml:"saltSize,attr"`
	BlockSize       int    `xml:"blockSize,attr"`
	KeyBits         int    `xml:"keyBits,attr"`
	HashSize        int    `xml:"hashSize,attr"`
	CipherAlgorithm string `xml:"cipherAlgorithm,attr"`
	CipherChaining  string `xml:"cipherChaining,attr"`
	HashAlgorithm   string `xml:"hashAlgorithm,attr"`
	SaltValue       string `xml:"saltValue,attr"`
}

type agileInfo struct {
	KeyData       agileParams `xml:"keyData"`
	KeyEncryptors []struct {
		URI          string `xml:"uri,attr"`
		EncryptedKey *struct {
			agileParams
			SpinCount                  int    `xml:"spinCount,attr"`
			EncryptedVerifierHashInput string `xml:"encryptedVerifierHashInput,attr"`
			EncryptedVerifierHashValue string `xml:"encryptedVerifierHashValue,attr"`
			EncryptedKeyValue          string `xml:"encryptedKeyValue,attr"`
		} `xml:"encryptedKey"`
	} `xml:"keyEncryptors>keyEncryptor"`
}

// 各项密钥推导用的固定块（MS-OFFCRYPTO 2.3.4.11、2.3.4.13）
var (
	agileVerifierInputBlock = []byte{0xfe, 0xa7, 0xd2, 0x76, 0x3b, 0x4b, 0x9e, 0x79}
	agileVerifierValueBlock = []byte{0xd7, 0xaa, 0x0f, 0x6d, 0x30, 0x61, 0x34, 0x4e}
	agileKeyValueBlock      = []byte{0x14, 0x6e, 0x0b, 0xe7, 0xab, 0xac, 0xd0, 0xd6}
)

func agileHash(name string) func() hash.Hash {
	switch strings.ToUpper(name) {
	case "SHA1", "SHA-1":
		return sha1.New
	case "SHA256", "SHA-256":
		return sha256.New
	case "SHA384", "SHA-384":
		return sha512.New384
	case "SHA512", "SHA-512":
		return sha512.New
	case "MD5":
		return md5.New
	}
	return nil
}

// fitBytes 把 b 截短或用 0x36 补齐到 n 字节（MS-OFFCRYPTO 2.3.4.11）。
func fitBytes(b []byte, n int) []byte {
	if len(b) >= n {
		return b[:n]
	}
	out := append([]byte{}, b...)
	for len(out) < n {
		out = append(out, 0x36)
	}
	return out
}

func agileDecrypt(x, pkg []byte, password string) ([]byte, error) {
	var ai agileInfo
	if err := xml.Unmarshal(bytes.TrimRight(x, "\x00"), &ai); err != nil {
		return nil, errOOXMLUnsupported
	}
	kd := ai.KeyData
	var ek *agileParams
	var spin int
	var encInput, encValue, encKey string
	for _, e := range ai.KeyEncryptors {
		if e.EncryptedKey != nil && strings.HasSuffix(e.URI, "/password") {
			ek, spin = &e.EncryptedKey.agileParams, e.EncryptedKey.SpinCount
			encInput, encValue, encKey = e.EncryptedKey.EncryptedVerifierHashInput, e.EncryptedKey.EncryptedVerifierHashValue, e.EncryptedKey.EncryptedKeyValue
			break
		}
	}
	if ek == nil { // 只有证书加密
		return nil, errOOXMLUnsupported
	}
	for _, p := range []*agileParams{&kd, ek} {
		if !strings.EqualFold(p.CipherAlgorithm, "AES") || !strings.EqualFold(p.CipherChaining, "ChainingModeCBC") ||
			agileHash(p.HashAlgorithm) == nil || (p.KeyBits != 128 && p.KeyBits != 192 && p.KeyBits != 256) || p.BlockSize != aes.BlockSize {
			return nil, errOOXMLUnsupported
		}
	}
	if spin < 0 || spin > 10_000_000 || ek.SaltSize <= 0 || ek.HashSize <= 0 || ek.HashSize > agileHash(ek.HashAlgorithm)().Size() {
		return nil, errOOXMLUnsupported
	}
	dec := func(s string) []byte { b, _ := base64.StdEncoding.DecodeString(s); return b }
	salt, input, value, keyValue, dataSalt := dec(ek.SaltValue), dec(encInput), dec(encValue), dec(encKey), dec(kd.SaltValue)
	if len(salt) == 0 || len(dataSalt) == 0 {
		return nil, errOOXMLUnsupported
	}

	newHash := agileHash(ek.HashAlgorithm)
	hf := newHash()
	hf.Write(salt)
	hf.Write(utf16leBytes(password))
	h := hf.Sum(nil)
	var it [4]byte
	for i := 0; i < spin; i++ {
		binary.LittleEndian.PutUint32(it[:], uint32(i))
		hf.Reset()
		hf.Write(it[:])
		hf.Write(h)
		h = hf.Sum(h[:0])
	}
	cbc := func(blockKey, data []byte) ([]byte, error) {
		hf.Reset()
		hf.Write(h)
		hf.Write(blockKey)
		key := fitBytes(hf.Sum(nil), ek.KeyBits/8)
		return cbcDecrypt(key, fitBytes(salt, ek.BlockSize), data)
	}
	vin, err1 := cbc(agileVerifierInputBlock, input)
	vval, err2 := cbc(agileVerifierValueBlock, value)
	kval, err3 := cbc(agileKeyValueBlock, keyValue)
	if err1 != nil || err2 != nil || err3 != nil || len(vin) < ek.SaltSize || len(vval) < ek.HashSize || len(kval) < kd.KeyBits/8 {
		return nil, errOOXMLUnsupported
	}
	hv := newHash()
	hv.Write(vin[:ek.SaltSize])
	if !bytes.Equal(hv.Sum(nil)[:ek.HashSize], vval[:ek.HashSize]) {
		return nil, errOOXMLWrongPassword
	}
	secret := kval[:kd.KeyBits/8]

	// 数据按 4096 字节分段，每段的 IV = Hash(keyData.salt + 段号)
	size := binary.LittleEndian.Uint64(pkg)
	data := pkg[8:]
	if size > uint64(len(data)) {
		return nil, errOOXMLUnsupported
	}
	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, errOOXMLUnsupported
	}
	out := make([]byte, 0, len(data))
	dh := agileHash(kd.HashAlgorithm)()
	for seg, off := uint32(0), 0; off < len(data); seg, off = seg+1, off+4096 {
		end := min(off+4096, len(data))
		chunk := data[off:end]
		chunk = chunk[:len(chunk)/aes.BlockSize*aes.BlockSize]
		dh.Reset()
		dh.Write(dataSalt)
		binary.LittleEndian.PutUint32(it[:], seg)
		dh.Write(it[:])
		iv := fitBytes(dh.Sum(nil), kd.BlockSize)
		plain := make([]byte, len(chunk))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, chunk)
		out = append(out, plain...)
		if uint64(len(out)) >= size {
			break
		}
	}
	if uint64(len(out)) < size {
		return nil, errOOXMLUnsupported
	}
	return out[:size], nil
}

func cbcDecrypt(key, iv, data []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	data = data[:len(data)/aes.BlockSize*aes.BlockSize]
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(b, iv).CryptBlocks(out, data)
	return out, nil
}

// ---- 提取 ----

// oleHasDRM：复合文档里有权限管理（IRM / 敏感度标签）的数据空间。
func oleHasDRM(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	d, err := mscfb.New(f)
	if err != nil {
		return false
	}
	for _, e := range d.File {
		if strings.Contains(e.Name, "DRM") {
			return true
		}
	}
	return false
}

// extractEncryptedOOXML 处理加密的 docx/xlsx/pptx：先用默认密码解密，解开后照常提取。
// fam 是 "doc"、"xls" 或 "ppt"。
func extractEncryptedOOXML(p, fam string) doc.Result {
	if oleHasDRM(p) {
		return doc.Result{Status: doc.StatusEncrypted, Err: msgIRM}
	}
	s, _, err := oleStreams(p, "EncryptionInfo", "EncryptedPackage")
	if err != nil || s["EncryptionInfo"] == nil || s["EncryptedPackage"] == nil {
		return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密"}
	}
	plain, err := ooxmlDecrypt(s["EncryptionInfo"], s["EncryptedPackage"], xlsDefaultPassword)
	switch {
	case errors.Is(err, errOOXMLWrongPassword):
		return doc.Result{Status: doc.StatusEncrypted, Err: msgNeedPassword}
	case err != nil:
		return doc.Result{Status: doc.StatusEncrypted, Err: "文件已加密（" + err.Error() + "）"}
	}
	ext := map[string]string{"doc": "docx", "xls": "xlsx", "ppt": "pptx"}[fam]
	tmp, err := os.CreateTemp("", "zhaozhao-dec-*."+ext)
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: "无法写临时文件: " + err.Error()}
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(plain)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return doc.Result{Status: doc.StatusFailed, Err: "无法写临时文件: " + err.Error()}
	}
	switch fam {
	case "doc":
		return extractDocx(tmp.Name())
	case "xls":
		return extractXlsx(tmp.Name())
	}
	return extractPptx(tmp.Name())
}
