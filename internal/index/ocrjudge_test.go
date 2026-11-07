package index

import (
	"context"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/extract"
)

// 多认出的内容：字典里常见、另一个引擎也认到的判为真内容；字典里没有、另一个引擎也没有的判为乱码。
func TestJudgeExtras(t *testing.T) {
	pool := []string{
		"关于做好安全生产检查工作的通知各单位要认真落实责任制度",
		"本合同自双方签字盖章之日起生效有效期为三年期满自动续签",
		"项目资金使用情况审计报告显示总体规范但存在部分问题需要整改",
		"会议纪要记录了各部门负责人对年度工作计划的意见和建议",
		"请于本月底前将统计报表报送办公室逾期未报的按规定处理",
		"施工图纸经设计单位复核后交建设单位存档备查并抄送监理",
	}
	old := ocrTestDict
	t.Cleanup(func() { ocrTestDict = old })
	ocrTestDict = func(ctx context.Context, m *Manager, fn func(string)) int {
		for i := range 80 { // 每份文字有其中几句
			fn(pool[i%6] + "。" + pool[(i+1)%6] + "。" + pool[(i+3)%6])
		}
		return 80
	}
	base := pool[0] + pool[1]
	realExtra := pool[2] + pool[3] + pool[4]
	junk := "蕤鼯戤祓䶮彧爨鬻龖靐齉籥麤蠡鸑饕餮魑魅魍魉夔犇骉羴猋淼焱垚赑屃椭圆曦曌"
	texts := []map[string]string{
		{"v6": base, "win": base + realExtra, "v6big": base + pool[2] + pool[3]},
		{"v6": base, "win": base + junk + junk[3:] + junk[7:], "v6big": base},
		{"v6": base, "win": base + "通知"}, // 多认的太少，不判断
	}
	r := &OCRTestReport{Base: "v6"}
	for range texts {
		r.Files = append(r.Files, OCRTestFile{Items: []OCRTestFileItem{{Model: "v6"}, {Model: "v6big"}, {Model: "win"}}})
	}
	r.Models = []OCRTestModel{{Name: "v6"}, {Name: "v6big"}, {Name: "win"}}
	m := &Manager{}
	m.judgeExtras(context.Background(), r, texts, []string{"v6", extract.OCRModelV6Big, extract.OCRModelWin})
	real, fake, few := r.Files[0].Items[2], r.Files[1].Items[2], r.Files[2].Items[2]
	if real.Verdict != verdictReal || real.ExtraOther < 50 || real.ExtraReal < 80 {
		t.Fatalf("真内容：%+v", real)
	}
	if fake.Verdict != verdictFake || fake.ExtraOther != 0 || fake.ExtraReal > 10 {
		t.Fatalf("乱码：%+v", fake)
	}
	if few.Verdict != "" || few.Extra != 1 {
		t.Fatalf("多认的太少：%+v", few)
	}
	win := r.Models[2]
	if r.DictDocs != 80 || win.RealFiles != 1 || win.FakeFiles != 1 || win.ExtraOther <= 0 || win.ExtraReal < 0 {
		t.Fatalf("汇总：%d %+v", r.DictDocs, win)
	}
	// 大图找字多认的内容，拿 Windows 自带 OCR 当另一个引擎
	if big := r.Files[0].Items[1]; big.Verdict != verdictReal || big.ExtraOther != 100 {
		t.Fatalf("大图找字：%+v", big)
	}
	s := OCRTestText(r, false)
	if !strings.Contains(s, "PP-OCRv6 small（大图找字）也认到") || !strings.Contains(s, "真内容 1 个、像乱码 1 个") || !strings.Contains(s, "随机 80 份") {
		t.Fatal(s)
	}
}
