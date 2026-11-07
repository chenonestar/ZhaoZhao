const ROWS: [string, string, string][] = [
  ["空格分隔", "同时包含（AND）", "合同 违约金"],
  ["\"…\"", "完整短语，按原顺序连续出现", "\"不可抗力条款\""],
  ["OR", "任一包含", "报销 OR 差旅"],
  ["- 或 NOT", "不包含", "张三 报销 -草稿"],
  ["( )", "分组", "(合同 OR 协议) 2025"],
  ["NEAR/n", "两个词相距不超过 n 个字，不限先后", "甲方 NEAR/10 违约"],
  ["~ 后缀", "模糊匹配（英文按编辑距离，中文按邻近）", "合同审批~"],
  ["/…/", "正则表达式（RE2 语法）", "/1[3-9]\\d{9}/"],
  ["path:", "路径包含（含压缩包和邮件内部路径）", "path:D:\\项目\\2025"],
  ["ext: / type:", "扩展名 / 类别（文档 表格 演示 PDF 图片 文本 代码 压缩包 邮件）", "ext:xlsx   type:邮件"],
  ["name:", "只匹配文件名", "name:周报"],
  ["from: to: subject:", "邮件发件人 / 收件人 / 主题", "from:张三  subject:报销"],
  ["modified:", "修改时间（邮件按发送日期）。<30d 为最近 30 天内，>30d 为 30 天之前", "modified:2025-01..2025-06   modified:<30d"],
  ["size:", "文件大小", "size:>10MB   size:1MB..5MB"],
  ["vol:", "所在卷（按卷标）", "vol:备份盘A"],
];

export function Help() {
  return (
    <div className="help">
      <h4>查询语法</h4>
      <table className="grid">
        <thead><tr><th>写法</th><th>含义</th><th>示例</th></tr></thead>
        <tbody>
          {ROWS.map((r) => (
            <tr key={r[0]}><td><code>{r[0]}</code></td><td>{r[1]}</td><td><code>{r[2]}</code></td></tr>
          ))}
        </tbody>
      </table>
      <h4>匹配规则</h4>
      <ul>
        <li>单个汉字、两个字的中文词都能精确搜到。</li>
        <li>英文大小写、全角与半角、中文标点的全半角形式始终忽略；繁简体归一默认开启，可在设置中关闭。</li>
        <li>英文和数字按词的开头匹配（<code>inv</code> 能命中 <code>invoice</code>）。</li>
        <li>标点、空白、换行不影响连续：<code>合同</code> 能命中被换行隔开的「合」「同」。</li>
        <li>模糊：5–8 位英文数字允许 1 处差异，9 位以上允许 2 处；中文要求字按顺序出现，相邻字之间最多夹 2 个字。模糊结果排在精确结果之后。</li>
        <li>正则采用 RE2 语法，<b>不支持反向引用和环视</b>；默认不区分大小写，可用 <code>(?s)</code> 让 <code>.</code> 跨行。正则查询需按回车执行；表达式里含有汉字时会先用索引缩小范围，否则要扫描全部已提取的文本。</li>
      </ul>
      <h4>快捷键</h4>
      <ul>
        <li><code>↑ ↓</code> 选择结果，<code>Enter</code> 打开文件，<code>Ctrl+Enter</code> 打开所在文件夹，<code>Ctrl+C</code>（输入框无选中文字时）复制路径。</li>
        <li><code>F3</code> / <code>Shift+F3</code> 预览中跳到下一处 / 上一处命中。<code>Esc</code> 清空搜索，再按一次隐藏窗口。</li>
      </ul>
    </div>
  );
}
