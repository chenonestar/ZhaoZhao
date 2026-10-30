package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

func TestVersionNums(t *testing.T) {
	for in, want := range map[string][4]uint16{"0.2.11": {0, 2, 11, 0}, "0.2.11-3-gabc": {0, 2, 11, 3}, "dev": {}, "1.2": {1, 2, 0, 0}} {
		if got := versionNums(in); got != want {
			t.Errorf("%s：%v", in, got)
		}
	}
}

// 生成的目标文件：x64、一个 .rsrc 段、每个资源一个重定位；资源数据原样在段里。
func TestCOFF(t *testing.T) {
	ico, err := os.ReadFile("../../icon/zhaozhao.ico")
	if err != nil {
		t.Fatal(err)
	}
	res, err := iconResources(ico)
	if err != nil {
		t.Fatal(err)
	}
	res = append(res, resource{rtVersion, 1, langZhCN, versionInfo("0.2.11", versionNums("0.2.11"))})
	b := coff(res)
	le := binary.LittleEndian
	if le.Uint16(b) != 0x8664 || le.Uint16(b[2:]) != 1 || string(b[20:25]) != ".rsrc" {
		t.Fatal("文件头不对")
	}
	if n := le.Uint16(b[20+32:]); int(n) != len(res) {
		t.Fatalf("重定位 %d 个，资源 %d 个", n, len(res))
	}
	for _, r := range res {
		if !bytes.Contains(b, r.data) {
			t.Fatalf("资源 %d/%d 的数据不在文件里", r.typ, r.id)
		}
	}
	if len(res) != 10 { // 8 个尺寸的图标 + 图标组 + 版本信息
		t.Fatalf("%d 个资源", len(res))
	}
}
