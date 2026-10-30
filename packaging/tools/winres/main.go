// winres 生成 Windows 资源文件 cmd/zhaozhao/rsrc_windows_amd64.syso：程序图标（packaging/icon/zhaozhao.ico）、
// 版本信息（文件属性里看到的版本号、说明）和清单（高分辨率屏幕不模糊、用新式系统对话框）。Go 编译 Windows 版时
// 自动把同目录的 .syso 链接进去。只用标准库，打包和 Windows 测试前运行：
//
//	go run ./packaging/tools/winres -version 0.2.11
//
// .syso 是 COFF 目标文件，里面一个 .rsrc 段：资源目录树（类型 → 编号 → 语言）、数据项和数据，数据项里的地址
// 用 IMAGE_REL_AMD64_ADDR32NB 重定位，由链接器换成最终的地址（和 rsrc、go-winres 生成的格式相同）。
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
	rtManifest  = 24

	langNeutral = 0
	langZhCN    = 0x0804

	// IconID 是程序图标组的编号（托盘用 LoadImage 按这个编号取，见 cmd/zhaozhao/shell_windows.go）。
	iconID = 1
)

const manifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity type="win32" name="ZhaoZhao" version="%s" processorArchitecture="amd64"/>
  <description>找找</description>
  <dependency>
    <dependentAssembly>
      <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
    </dependentAssembly>
  </dependency>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security><requestedPrivileges><requestedExecutionLevel level="asInvoker" uiAccess="false"/></requestedPrivileges></security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
      <supportedOS Id="{1f676c76-80e1-4239-95bb-83d0f6d0da78}"/>
      <supportedOS Id="{4a2f28e3-53b9-4441-ba9c-d69d4a4a6e38}"/>
      <supportedOS Id="{35138b9a-5d96-4fbd-8e2d-a2440225f93a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true/pm</dpiAware>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2, PerMonitor</dpiAwareness>
    </windowsSettings>
  </application>
</assembly>
`

type resource struct {
	typ, id, lang uint16
	data          []byte
}

func main() {
	ver := flag.String("version", "0.0.0", "版本号（如 0.2.11 或 v0.2.11）")
	icon := flag.String("icon", "packaging/icon/zhaozhao.ico", "图标文件")
	out := flag.String("o", "cmd/zhaozhao/rsrc_windows_amd64.syso", "输出文件")
	flag.Parse()
	v := strings.TrimPrefix(*ver, "v")
	nums := versionNums(v)
	ico, err := os.ReadFile(*icon)
	if err != nil {
		fail(err)
	}
	res, err := iconResources(ico)
	if err != nil {
		fail(fmt.Errorf("%s：%v", *icon, err))
	}
	res = append(res,
		resource{rtVersion, 1, langZhCN, versionInfo(v, nums)},
		resource{rtManifest, 1, langNeutral, []byte(fmt.Sprintf(manifest, fmt.Sprintf("%d.%d.%d.%d", nums[0], nums[1], nums[2], nums[3])))})
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(*out, coff(res), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("已生成 %s（版本 %s，%d 个资源）\n", *out, v, len(res))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "winres:", err)
	os.Exit(1)
}

// versionNums 取版本号里的前四个数字（0.2.11-3-gabc → 0,2,11,0）。
func versionNums(v string) [4]uint16 {
	var n [4]uint16
	for i, p := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' }) {
		if i >= 4 {
			break
		}
		x, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			break
		}
		n[i] = uint16(x)
	}
	return n
}

// iconResources 把 .ico 文件拆成一个个图标（RT_ICON，编号从 1 起）和图标组（RT_GROUP_ICON）。
// .ico 的目录项最后 4 字节是数据在文件里的位置，图标组里换成 2 字节的图标编号。
func iconResources(ico []byte) ([]resource, error) {
	if len(ico) < 6 || binary.LittleEndian.Uint16(ico[2:]) != 1 {
		return nil, fmt.Errorf("不是图标文件")
	}
	n := int(binary.LittleEndian.Uint16(ico[4:]))
	if len(ico) < 6+16*n {
		return nil, fmt.Errorf("图标文件不完整")
	}
	var out []resource
	group := bytes.NewBuffer(nil)
	group.Write([]byte{0, 0, 1, 0})
	binary.Write(group, binary.LittleEndian, uint16(n))
	for i := range n {
		e := ico[6+16*i : 6+16*i+16]
		size, off := binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])
		if uint64(off)+uint64(size) > uint64(len(ico)) {
			return nil, fmt.Errorf("第 %d 个图像超出文件", i+1)
		}
		out = append(out, resource{rtIcon, uint16(i + 1), langNeutral, ico[off : off+size]})
		group.Write(e[:12])
		binary.Write(group, binary.LittleEndian, uint16(i+1))
	}
	return append(out, resource{rtGroupIcon, iconID, langNeutral, group.Bytes()}), nil
}

func utf16z(s string) []byte {
	var b bytes.Buffer
	for _, c := range utf16.Encode([]rune(s)) {
		binary.Write(&b, binary.LittleEndian, c)
	}
	b.Write([]byte{0, 0})
	return b.Bytes()
}

func pad4(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

// verNode 写版本信息里的一个节点：长度、值长度、类型（1 文字，0 二进制）、名字、值、子节点，都按 4 字节对齐。
// 文字值的「值长度」按字符数（含结尾的 0）算。
func verNode(key string, value []byte, text bool, children ...[]byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0, 0, 0, 0, 0, 0}) // 长度、值长度、类型，最后填
	b.Write(utf16z(key))
	pad4(&b)
	b.Write(value)
	for _, c := range children {
		pad4(&b)
		b.Write(c)
	}
	out := b.Bytes()
	vlen, typ := len(value), uint16(0)
	if text {
		vlen, typ = len(value)/2, 1
	}
	binary.LittleEndian.PutUint16(out[0:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[2:], uint16(vlen))
	binary.LittleEndian.PutUint16(out[4:], typ)
	return out
}

// versionInfo 生成版本信息（VS_VERSIONINFO）：文件属性的「详细信息」里显示的版本号、说明。
func versionInfo(v string, n [4]uint16) []byte {
	var fixed bytes.Buffer
	ms, ls := uint32(n[0])<<16|uint32(n[1]), uint32(n[2])<<16|uint32(n[3])
	for _, x := range []uint32{0xFEEF04BD, 0x00010000, ms, ls, ms, ls, 0x3F, 0, 0x40004, 1, 0, 0, 0} {
		binary.Write(&fixed, binary.LittleEndian, x)
	}
	var strs [][]byte
	for _, kv := range [][2]string{
		{"CompanyName", "找找"},
		{"FileDescription", "找找 文件全文搜索"},
		{"FileVersion", v},
		{"InternalName", "zhaozhao"},
		{"LegalCopyright", "找找 ZhaoZhao"},
		{"OriginalFilename", "zhaozhao.exe"},
		{"ProductName", "找找"},
		{"ProductVersion", v},
	} {
		strs = append(strs, verNode(kv[0], utf16z(kv[1]), true))
	}
	table := verNode("080404b0", nil, true, strs...)
	sfi := verNode("StringFileInfo", nil, true, table)
	trans := []byte{0x04, 0x08, 0xB0, 0x04} // 中文（简体），Unicode
	vfi := verNode("VarFileInfo", nil, true, verNode("Translation", trans, false))
	return verNode("VS_VERSION_INFO", fixed.Bytes(), false, sfi, vfi)
}

// coff 把资源写成只有一个 .rsrc 段的 COFF 目标文件（x64）。
func coff(res []resource) []byte {
	slices.SortFunc(res, func(a, b resource) int {
		if a.typ != b.typ {
			return int(a.typ) - int(b.typ)
		}
		if a.id != b.id {
			return int(a.id) - int(b.id)
		}
		return int(a.lang) - int(b.lang)
	})
	// 目录树：每个类型、每个（类型，编号）各一个目录，每个资源一个语言目录项 → 数据项
	var types []uint16
	ids := map[uint16][]uint16{}
	for _, r := range res {
		if !slices.Contains(types, r.typ) {
			types = append(types, r.typ)
		}
		if !slices.Contains(ids[r.typ], r.id) {
			ids[r.typ] = append(ids[r.typ], r.id)
		}
	}
	dirSize := func(entries int) int { return 16 + 8*entries }
	// 先算各部分的位置：根目录、类型目录、编号目录、数据项、数据
	off := dirSize(len(types))
	typeOff := map[uint16]int{}
	for _, t := range types {
		typeOff[t] = off
		off += dirSize(len(ids[t]))
	}
	idOff := map[[2]uint16]int{}
	for _, t := range types {
		for _, id := range ids[t] {
			idOff[[2]uint16{t, id}] = off
			n := 0
			for _, r := range res {
				if r.typ == t && r.id == id {
					n++
				}
			}
			off += dirSize(n)
		}
	}
	entryOff := make([]int, len(res))
	for i := range res {
		entryOff[i] = off
		off += 16
	}
	dataOff := make([]int, len(res))
	for i, r := range res {
		off = (off + 7) &^ 7
		dataOff[i] = off
		off += len(r.data)
	}
	sec := make([]byte, (off+7)&^7)
	le := binary.LittleEndian
	writeDir := func(at, nIDs int) { le.PutUint16(sec[at+14:], uint16(nIDs)) }
	writeEntry := func(at int, id uint16, target int, sub bool) {
		le.PutUint32(sec[at:], uint32(id))
		v := uint32(target)
		if sub {
			v |= 0x80000000
		}
		le.PutUint32(sec[at+4:], v)
	}
	writeDir(0, len(types))
	for i, t := range types {
		writeEntry(16+8*i, t, typeOff[t], true)
		writeDir(typeOff[t], len(ids[t]))
		for j, id := range ids[t] {
			writeEntry(typeOff[t]+16+8*j, id, idOff[[2]uint16{t, id}], true)
			k := 0
			for ri, r := range res {
				if r.typ == t && r.id == id {
					writeDir(idOff[[2]uint16{t, id}], 0) // 先写，下面再补个数
					writeEntry(idOff[[2]uint16{t, id}]+16+8*k, r.lang, entryOff[ri], false)
					k++
				}
			}
			writeDir(idOff[[2]uint16{t, id}], k)
		}
	}
	var relocs []uint32
	for i, r := range res {
		le.PutUint32(sec[entryOff[i]:], uint32(dataOff[i])) // 链接时加上段的地址
		le.PutUint32(sec[entryOff[i]+4:], uint32(len(r.data)))
		relocs = append(relocs, uint32(entryOff[i]))
		copy(sec[dataOff[i]:], r.data)
	}
	// 文件：文件头 20、段头 40、段数据、重定位（每个 10）、符号表（1 个，18）、字符串表（4）
	const hdr = 20 + 40
	relocAt := hdr + len(sec)
	symAt := relocAt + 10*len(relocs)
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, le, v) }
	w(uint16(0x8664))                  // Machine: x64
	w(uint16(1))                       // 段数
	w(uint32(0))                       // 时间
	w(uint32(symAt))                   // 符号表位置
	w(uint32(1))                       // 符号数
	w(uint16(0))                       // 可选头大小
	w(uint16(0))                       // 属性
	b.WriteString(".rsrc\x00\x00\x00") // 段名
	w(uint32(0))                       // VirtualSize
	w(uint32(0))                       // VirtualAddress
	w(uint32(len(sec)))                // 段数据大小
	w(uint32(hdr))                     // 段数据位置
	w(uint32(relocAt))                 // 重定位位置
	w(uint32(0))                       // 行号位置
	w(uint16(len(relocs)))             // 重定位个数
	w(uint16(0))                       // 行号个数
	w(uint32(0x40000040))              // 已初始化数据、可读
	b.Write(sec)
	for _, at := range relocs {
		w(at)        // 位置
		w(uint32(0)) // 符号：.rsrc 段
		w(uint16(3)) // IMAGE_REL_AMD64_ADDR32NB
	}
	b.WriteString(".rsrc\x00\x00\x00") // 符号名
	w(uint32(0))                       // 值
	w(uint16(1))                       // 所在段
	w(uint16(0))                       // 类型
	w(uint8(3))                        // IMAGE_SYM_CLASS_STATIC
	w(uint8(0))                        // 辅助符号数
	w(uint32(4))                       // 字符串表：只有长度
	return b.Bytes()
}
