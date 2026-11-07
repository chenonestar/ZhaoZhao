package extract

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenonestar/zhaozhao/internal/doc"
)

// zip 里各种压缩方式的条目都能读出（PPMd 写明不支持）；条目损坏时写明校验和不对，其他条目照常。
// 样本由 tools/make_archive_fixtures.py 生成（Deflate64、PPMd、zstd 的数据分别由 inflate64、pyppmd、zstandard 压缩）。
func TestZipCompressionMethods(t *testing.T) {
	res := Extract(filepath.Join("..", "..", "testdata", "archives", "多种压缩方式.zip"), "zip", Options{})
	if res.Status != doc.StatusOK {
		t.Fatalf("%v %s", res.Status, res.Err)
	}
	got := map[string]doc.Child{}
	for _, c := range res.Children {
		got[c.Inner] = c
	}
	for _, m := range []string{"存储", "deflate", "deflate64", "bzip2", "lzma", "zstd", "xz"} {
		c, ok := got["目录/"+m+".txt"]
		if !ok || c.Status != doc.StatusOK || !strings.Contains(c.Text, m+"关键词") || !strings.Contains(c.Text, m+"关键词尾巴") {
			t.Errorf("%s：%v %q %.60q", m, c.Status, c.Err, c.Text)
		}
	}
	if c := got["目录/ppmd.txt"]; c.Status != doc.StatusFailed || !strings.Contains(c.Err, "不支持的压缩方式 PPMd") {
		t.Errorf("ppmd：%v %q", c.Status, c.Err)
	}

	res = Extract(filepath.Join("..", "..", "testdata", "archives", "条目损坏.zip"), "zip", Options{})
	got = map[string]doc.Child{}
	for _, c := range res.Children {
		got[c.Inner] = c
	}
	if c := got["完好.txt"]; c.Status != doc.StatusOK || !strings.Contains(c.Text, "完好的条目") {
		t.Errorf("完好.txt：%v %q", c.Status, c.Err)
	}
	if c := got["损坏.txt"]; c.Status != doc.StatusFailed || !strings.Contains(c.Err, "损坏") {
		t.Errorf("损坏.txt 应注明数据损坏：%v %q", c.Status, c.Err)
	}
}

// 「打开」压缩包里用 Deflate64 压缩的文件：解出到临时目录。
func TestExtractInnerDeflate64(t *testing.T) {
	out, err := ExtractInner(filepath.Join("..", "..", "testdata", "archives", "多种压缩方式.zip"), "目录/deflate64.txt", t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := Extract(out, "txt", Options{})
	if !strings.Contains(r.Text, "deflate64关键词尾巴") {
		t.Fatalf("%.80q", r.Text)
	}
}
