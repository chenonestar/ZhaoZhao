找找 ZhaoZhao —— Windows 单机全文检索工具
=======================================

双击 zhaozhao.exe 即可运行。第一次打开后，点「索引与设置」→「添加目录」选择要建立索引的目录或整个盘符。

  * 全局快捷键 Ctrl+Alt+F 呼出搜索窗口，Esc 隐藏；常驻系统托盘。
  * 数据（索引、设置、日志）保存在本目录下的 data 文件夹（便携版）。
    安装版保存在 %LOCALAPPDATA%\ZhaoZhao。
  * 软件不联网，不收集任何使用数据。
  * 老格式（.doc/.xls/.ppt/.wps 等）和扫描件 OCR 需要 components 目录里的组件，见 components\README.txt。
  * 界面使用系统自带的 Microsoft Edge 以应用窗口形式显示（Windows 10 22H2 / Windows 11 均已自带），
    找不到 Edge 或 Chrome 时用默认浏览器打开。

命令行参数：
  -data <目录>   指定数据目录
  -add <目录>    启动时添加索引根（可重复）
  -no-ui         启动时不自动打开窗口（只常驻托盘）
  -port <端口>   指定界面服务端口（默认随机，仅监听 127.0.0.1）
