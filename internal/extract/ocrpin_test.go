package extract

import (
	"slices"
	"strings"
	"testing"
)

func TestPinMask(t *testing.T) {
	// 6 核 12 线程：前 6 个进程各占一个物理核心的第一个逻辑处理器，第 7 个起用第二个
	var got []string
	for k := 0; k < 8; k++ {
		got = append(got, maskText(pinMask(k, 1, 6, 12)))
	}
	if want := []string{"0", "2", "4", "6", "8", "10", "1", "3"}; !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if s := maskText(pinMask(1, 2, 6, 12)); s != "4、6" { // 第 2 个进程 2 线程：第 3、4 个物理核心
		t.Fatal(s)
	}
	if s := maskText(pinMask(5, 1, 4, 4)); s != "1" { // 没有超线程，超出的回绕
		t.Fatal(s)
	}
	env := strings.Join(tameEnv(2), " ")
	for _, w := range []string{"OMP_NUM_THREADS=2", "OMP_WAIT_POLICY=PASSIVE", "OPENCV_FOR_THREADS_NUM=1"} {
		if !strings.Contains(env, w) {
			t.Fatal(env)
		}
	}
	a, b := OCRConfig{Command: "x"}, OCRConfig{Command: "x", Tame: true}
	if a.key() == b.key() {
		t.Fatal("限制辅助线程要换一个 OCR 组件进程")
	}
}
