package extract

import (
	"fmt"
	"image"
	"strings"
	"testing"
)

func TestJoinWords(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"中", "华", "人民", "共和国"}, "中华人民共和国"},
		{[]string{"Hello", "world"}, "Hello world"},
		{[]string{"共", "3", "页", "PDF", "文件", "，", "2024", "年"}, "共3页PDF文件，2024年"},
		{[]string{"第", "12", "条"}, "第12条"},
		{nil, ""},
	} {
		if got := joinWords(c.in); got != c.want {
			t.Errorf("joinWords(%q) = %q，应为 %q", c.in, got, c.want)
		}
	}
}

// 认出的行按位置排：从上到下，同一排的从左到右（表格的一行不再被拆成一列一列地读）。
func TestOrderLines(t *testing.T) {
	ls := []winLine{
		{"金额", 300, 100, 360, 130}, {"姓名", 20, 102, 80, 128}, {"张三", 22, 150, 80, 178},
		{"1200", 300, 148, 360, 180}, {"标题", 100, 20, 300, 60},
	}
	if got := orderLines(ls); got != "标题\n姓名\n金额\n张三\n1200\n" {
		t.Fatalf("%q", got)
	}
}

func TestRotateGray(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 3, 2)) // 0 1 2 / 3 4 5
	for i := range g.Pix {
		g.Pix[i] = uint8(i)
	}
	get := func(g *image.Gray) []uint8 { return append([]uint8{}, g.Pix...) }
	if r := rotateGray(g, 90); r.Rect.Dx() != 2 || fmt.Sprint(get(r)) != "[3 0 4 1 5 2]" {
		t.Fatalf("90°：%v %v", r.Rect, get(r))
	}
	if r := rotateGray(g, 270); fmt.Sprint(get(r)) != "[2 5 1 4 0 3]" {
		t.Fatalf("270°：%v", get(r))
	}
	if r := rotateGray(g, 180); fmt.Sprint(get(r)) != "[5 4 3 2 1 0]" {
		t.Fatalf("180°：%v", get(r))
	}
	if r := rotateGray(rotateGray(g, 90), 270); fmt.Sprint(get(r)) != fmt.Sprint(get(g)) {
		t.Fatal("转 90° 再转 270° 应复原")
	}
}

// 方向不对的页转过来再认：横向页试 90°、270°；正常的竖向页只认一次。
func TestWinOCRBest(t *testing.T) {
	good := strings.Repeat("关于做好安全生产检查工作的通知", 6)
	fake := func(right int) func(image.Image) (string, error) {
		return func(img image.Image) (string, error) {
			b := img.Bounds()
			// 这张图转到「正确」方向时（竖向，且已经转对角度）认得出；用左上角像素记录转了多少
			if b.Dx() < b.Dy() && img.(*image.Gray).Pix[0] == uint8(right) {
				return good, nil
			}
			return "l1 ,: ' i", nil
		}
	}
	mark := func(w, h int, v uint8) *image.Gray {
		g := image.NewGray(image.Rect(0, 0, w, h))
		g.Pix[0] = v
		return g
	}
	calls := 0
	count := func(f func(image.Image) (string, error)) func(image.Image) (string, error) {
		return func(i image.Image) (string, error) { calls++; return f(i) }
	}
	// 竖向、认得好：只认一次
	if text, deg, _ := winOCRBest(mark(20, 30, 7), count(fake(7))); text != good || deg != 0 || calls != 1 {
		t.Fatalf("%d° %d 次", deg, calls)
	}
	// 横向页：转 90° 后（左下角的像素到了左上角）认得出
	g := mark(30, 20, 0)
	g.Pix[(20-1)*30] = 9 // 左下角，顺时针转 90° 后到左上角
	calls = 0
	if text, deg, _ := winOCRBest(g, count(fake(9))); text != good || deg != 90 || calls != 3 {
		t.Fatalf("横向页：%d° %d 次", deg, calls)
	}
	// 竖向但倒过来：转 180°
	g = mark(20, 30, 0)
	g.Pix[len(g.Pix)-1] = 5
	if _, deg, _ := winOCRBest(g, fake(5)); deg != 180 {
		t.Fatalf("倒着的页：%d°", deg)
	}
	// 哪个方向都认不出：保持原样
	if text, deg, _ := winOCRBest(mark(30, 20, 0), fake(99)); deg != 0 || text != "l1 ,: ' i" {
		t.Fatalf("%d° %q", deg, text)
	}
}
