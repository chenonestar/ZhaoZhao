package extract

import (
	"bytes"
	"crypto/md5"
	"crypto/rc4"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"unicode/utf16"
)

// Excel 97-2003 的加密（MS-XLS 2.2.10、MS-OFFCRYPTO 2.3.5/2.3.6）。
//
// 设了「保护工作簿」的 .xls 会被 Excel 用内置的默认密码 VelvetSweatshop 加密，打开时不需要输入密码。
// 内置解析器先用默认密码解密；默认密码不对的才是真正设了打开密码的文件。
// 支持两种 RC4 加密：老版本 Excel 的标准 RC4，以及 Excel 2003 以后默认的 RC4 CryptoAPI。
// 更老的 XOR 混淆交给 LibreOffice。

// xlsDefaultPassword 是 Excel「保护工作簿」时使用的默认密码。
const xlsDefaultPassword = "VelvetSweatshop"

var (
	errXlsWrongPassword = errors.New("密码不对")
	errXlsUnsupported   = errors.New("不支持的加密方式")
)

// xlsBlockKey 返回第 block 个 1024 字节块的 RC4 密钥。
type xlsBlockKey func(block uint32) []byte

func utf16leBytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

// xlsKey 按 FILEPASS 记录推导密钥并验证密码。
func xlsKey(filepass []byte, password string) (xlsBlockKey, error) {
	if len(filepass) < 6 || le16(filepass, 0) != 1 { // 0：XOR 混淆
		return nil, errXlsUnsupported
	}
	major, minor := le16(filepass, 2), le16(filepass, 4)
	pw := utf16leBytes(password)
	switch {
	case major == 1 && minor == 1: // 标准 RC4（40 位）
		if len(filepass) < 54 {
			return nil, errXlsUnsupported
		}
		salt, verifier, verifierHash := filepass[6:22], filepass[22:38], filepass[38:54]
		h0 := md5.Sum(pw)
		buf := make([]byte, 0, 16*21)
		for i := 0; i < 16; i++ {
			buf = append(buf, h0[:5]...)
			buf = append(buf, salt...)
		}
		h1 := md5.Sum(buf)
		key := func(block uint32) []byte {
			var b [9]byte
			copy(b[:5], h1[:5])
			binary.LittleEndian.PutUint32(b[5:], block)
			k := md5.Sum(b[:])
			return k[:]
		}
		if !xlsVerify(key(0), verifier, verifierHash, func(v []byte) []byte { h := md5.Sum(v); return h[:] }) {
			return nil, errXlsWrongPassword
		}
		return key, nil
	case major >= 2 && major <= 4 && minor == 2: // RC4 CryptoAPI
		if len(filepass) < 14 {
			return nil, errXlsUnsupported
		}
		hsize := le32(filepass, 10)
		hdr := 14
		if hsize < 32 || hdr+hsize > len(filepass) {
			return nil, errXlsUnsupported
		}
		algID, keyBits := le32(filepass, hdr+8), le32(filepass, hdr+16)
		if algID != 0 && algID != 0x6801 { // 只支持 RC4
			return nil, errXlsUnsupported
		}
		if keyBits == 0 {
			keyBits = 40
		}
		if keyBits < 40 || keyBits > 128 || keyBits%8 != 0 { // SHA-1 只有 160 位，RC4 最多 128 位
			return nil, errXlsUnsupported
		}
		v := hdr + hsize
		if v+4 > len(filepass) || le32(filepass, v) != 16 || v+4+16+16+4 > len(filepass) {
			return nil, errXlsUnsupported
		}
		salt := filepass[v+4 : v+20]
		verifier := filepass[v+20 : v+36]
		hashSize := le32(filepass, v+36)
		if hashSize != 20 || v+40+20 > len(filepass) {
			return nil, errXlsUnsupported
		}
		verifierHash := filepass[v+40 : v+60]
		h0 := sha1.Sum(append(append([]byte{}, salt...), pw...))
		key := func(block uint32) []byte {
			var b [24]byte
			copy(b[:20], h0[:])
			binary.LittleEndian.PutUint32(b[20:], block)
			h := sha1.Sum(b[:])
			if keyBits == 40 { // 40 位密钥补零到 128 位
				return append(append([]byte{}, h[:5]...), make([]byte, 11)...)
			}
			return h[:keyBits/8]
		}
		if !xlsVerify(key(0), verifier, verifierHash, func(v []byte) []byte { h := sha1.Sum(v); return h[:] }) {
			return nil, errXlsWrongPassword
		}
		return key, nil
	}
	return nil, errXlsUnsupported
}

// xlsVerify 用第 0 块的密钥解开验证数据（验证值和它的哈希在同一个 RC4 流里连续解密）。
func xlsVerify(key, verifier, verifierHash []byte, hash func([]byte) []byte) bool {
	c, err := rc4.NewCipher(key)
	if err != nil {
		return false
	}
	v := make([]byte, len(verifier))
	c.XORKeyStream(v, verifier)
	vh := make([]byte, len(verifierHash))
	c.XORKeyStream(vh, verifierHash)
	return bytes.Equal(hash(v), vh)
}

// xlsPlainRecords 是加密的 Workbook 流里不加密的记录（MS-XLS 2.2.10）。
var xlsPlainRecords = map[int]bool{
	0x0809: true, // BOF
	0x002F: true, // FILEPASS
	0x0194: true, // UsrExcl
	0x0195: true, // FileLock
	0x00E1: true, // InterfaceHdr
	0x0196: true, // RRDInfo
	0x0138: true, // RRDHead
}

// xlsDecrypt 解密 Workbook 流：按流内位置每 1024 字节换一个密钥；记录头（类型和长度）、
// 上面列出的记录、BoundSheet8 的前 4 字节（lbPlyPos）不加密，但同样占用密钥流的位置。
func xlsDecrypt(wb, filepass []byte, password string) ([]byte, error) {
	key, err := xlsKey(filepass, password)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(wb))
	for off := 0; off < len(wb); off += 1024 {
		end := min(off+1024, len(wb))
		c, err := rc4.NewCipher(key(uint32(off / 1024)))
		if err != nil {
			return nil, err
		}
		c.XORKeyStream(out[off:end], wb[off:end])
	}
	for off := 0; off+4 <= len(wb); {
		typ, n := le16(wb, off), le16(wb, off+2)
		copy(out[off:off+4], wb[off:off+4])
		end := min(off+4+n, len(wb))
		switch {
		case xlsPlainRecords[typ]:
			copy(out[off+4:end], wb[off+4:end])
		case typ == 0x0085: // BoundSheet8
			copy(out[off+4:min(off+8, end)], wb[off+4:min(off+8, end)])
		}
		off += 4 + n
	}
	return out, nil
}
