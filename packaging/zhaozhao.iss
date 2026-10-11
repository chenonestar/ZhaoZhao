; Inno Setup 脚本：生成离线完整安装包（需先运行 packaging/build-windows.sh，并把组件放进 components）
; 编译：iscc packaging\zhaozhao.iss
#define AppVer GetEnv("ZZ_VERSION")
#if AppVer == ""
  #define AppVer "0.1.0"
#endif

[Setup]
AppId={{6B2F5C58-1B6A-4C0E-9C57-ZHAOZHAO0001}
AppName=找找
AppVersion={#AppVer}
AppPublisher=ZhaoZhao
DefaultDirName={autopf}\ZhaoZhao
DefaultGroupName=找找
OutputDir=..\dist
OutputBaseFilename=ZhaoZhao-setup-{#AppVer}
Compression=lzma2
SolidCompression=yes
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0.19045
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
WizardStyle=modern
UninstallDisplayIcon={app}\zhaozhao.exe

[Languages]
Name: "chinesesimplified"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"

[Tasks]
Name: "autostart"; Description: "开机自动启动（常驻托盘）"; Flags: unchecked
Name: "desktopicon"; Description: "创建桌面快捷方式"; Flags: unchecked

[Files]
Source: "..\dist\ZhaoZhao-portable\*"; DestDir: "{app}"; Excludes: "portable.flag"; Flags: recursesubdirs createallsubdirs ignoreversion

[Icons]
Name: "{group}\找找"; Filename: "{app}\zhaozhao.exe"
Name: "{autodesktop}\找找"; Filename: "{app}\zhaozhao.exe"; Tasks: desktopicon

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "ZhaoZhao"; ValueData: """{app}\zhaozhao.exe"" -no-ui"; Tasks: autostart; Flags: uninsdeletevalue

[Run]
Filename: "{app}\zhaozhao.exe"; Description: "运行找找"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "taskkill"; Parameters: "/F /IM zhaozhao.exe"; Flags: runhidden; RunOnceId: "KillZhaoZhao"

[Code]
// 卸载时询问是否保留索引数据
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
  begin
    if MsgBox('是否保留索引数据（%LOCALAPPDATA%\ZhaoZhao）？' + #13#10 + '选「否」会删除全部索引、设置和日志。',
              mbConfirmation, MB_YESNO or MB_DEFBUTTON1) = IDNO then
      DelTree(ExpandConstant('{localappdata}\ZhaoZhao'), True, True, True);
  end;
end;
