//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 真的启动一个便携版找找（另一个文件夹、另一个数据目录），检测能找到它、取到版本号，并能请它退出。
func TestRunningOthersReal(t *testing.T) {
	if testing.Short() {
		t.Skip("要编译并启动找找")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "zhaozhao.exe")
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("编译失败：%v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "portable.flag"), nil, 0o600)
	cmd := exec.Command(exe, "-no-ui")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	data := filepath.Join(dir, "data")
	var found []otherInstance
	for i := 0; i < 150 && len(found) == 0; i++ { // 最多等 30 秒
		time.Sleep(200 * time.Millisecond)
		startNotes = nil
		found = runningOthers(t.TempDir())
	}
	if len(found) != 1 {
		t.Fatalf("应找到 1 个：%+v；经过：%q", found, startNotes)
	}
	o := found[0]
	if o.PID != cmd.Process.Pid || o.Version != "v9.9.9" || !samePath(o.DataDir, data) || !samePath(o.Exe, exe) {
		t.Fatalf("%+v", o)
	}
	if got := runningOthers(data); len(got) != 0 {
		t.Fatalf("同一个数据目录的不算别的找找：%+v", got)
	}
	if !instanceRunning(data) {
		t.Fatal("按进程号应认出它在用那个数据目录")
	}
	if ok, why := quitInstance(o); !ok {
		t.Fatal("请它退出后 20 秒内应结束：" + why)
	}
	if instanceRunning(data) {
		t.Fatal("退出后不应再算在运行")
	}
}

// 程序文件里的版本信息：系统文件一定有。
func TestExeVersion(t *testing.T) {
	v := exeVersion(filepath.Join(os.Getenv("SystemRoot"), "System32", "kernel32.dll"))
	if strings.Count(v, ".") < 2 {
		t.Fatalf("kernel32.dll 的版本：%q", v)
	}
	if v := exeVersion(filepath.Join(t.TempDir(), "没有.exe")); v != "" {
		t.Fatalf("没有的文件：%q", v)
	}
}
