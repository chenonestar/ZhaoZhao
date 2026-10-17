"""生成 OCR 测速用的样张 internal/index/ocrsample.png：A4、200 dpi 的灰度扫描页（正文 + 表格）。

找找在每台电脑上用它比较几种「进程数 × 线程数」搭配的速度。内容是虚构的通知，字号、行距、页边距
与常见公文扫描件相当（正文约 10.5 磅）。需要 Pillow 和文泉驿正黑字体：
    python3 tools/make_ocr_sample.py internal/index/ocrsample.png
"""
import sys

from PIL import Image, ImageDraw, ImageFont

FONT = "/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc"
W, H = 1654, 2339  # A4 @ 200 dpi
LEFT, RIGHT, TOP = 150, 150, 170

SENTENCES = [
    "为进一步规范档案管理工作，提高档案利用效率，经研究决定，于2025年第四季度开展档案数字化专项工作。",
    "各部门应于10月31日前完成本部门纸质档案的清点、整理和登记，并按照统一的编号规则填写移交清单。",
    "数字化扫描分辨率不低于200 dpi，彩色文件采用24位色彩模式，黑白文件可采用灰度模式保存。",
    "扫描完成后应逐页核对，确保页码连续、图像清晰、无遗漏、无重复，发现问题及时补扫。",
    "涉及合同、协议、发票等重要凭证的，应同时记录合同编号、签订日期、金额和对方单位名称。",
    "档案室负责建立电子目录，支持按文件名称、责任部门、形成年度和关键词进行全文检索。",
    "项目经费由综合管理部统一申请，预算总额为人民币386,500元，具体明细见附表。",
    "工作中如遇疑难问题，请及时与档案室联系，联系人：王晓明，电话：010-6852 3901。",
    "各单位要高度重视，明确专人负责，严格遵守保密规定，确保档案实体和数字信息安全。",
    "本通知自印发之日起施行，原《档案借阅管理办法（试行）》同时废止。",
]


def wrap(draw, text, font, width):
    lines, cur = [], ""
    for ch in text:
        if draw.textlength(cur + ch, font=font) > width and ch not in "。，、；：）》":  # 标点不放行首
            lines.append(cur)
            cur = ch
        else:
            cur += ch
    if cur:
        lines.append(cur)
    return lines


def main(out):
    img = Image.new("L", (W, H), 255)
    d = ImageDraw.Draw(img)
    title = ImageFont.truetype(FONT, 46)
    body = ImageFont.truetype(FONT, 30)
    small = ImageFont.truetype(FONT, 26)

    t = "关于开展2025年度档案数字化工作的通知"
    d.text(((W - d.textlength(t, font=title)) / 2, TOP), t, font=title, fill=0)
    d.text((LEFT, TOP + 110), "各部门、各直属单位：", font=body, fill=0)
    y = TOP + 170
    width = W - LEFT - RIGHT
    for i in range(14):
        para = SENTENCES[i % len(SENTENCES)] + SENTENCES[(i * 3 + 1) % len(SENTENCES)]
        lines = wrap(d, "　　" + para, body, width)
        for ln in lines:
            d.text((LEFT, y), ln, font=body, fill=0)
            y += 50
        if y > 1650:
            break
        y += 10

    # 附表：经费明细
    y += 20
    d.text((LEFT, y), "附表：经费明细", font=body, fill=0)
    y += 55
    cols = [LEFT, LEFT + 120, LEFT + 640, LEFT + 900, W - RIGHT]
    rows = [("序号", "项目", "数量", "金额（元）"), ("1", "扫描设备租赁", "2台", "96,000"),
            ("2", "外包扫描服务", "120,000页", "216,000"), ("3", "存储服务器扩容", "1套", "58,500"),
            ("4", "培训及其他", "—", "16,000")]
    top = y
    for r in rows:
        for k, cell in enumerate(r):
            d.text((cols[k] + 14, y + 10), cell, font=small, fill=0)
        y += 52
    for k in range(len(rows) + 1):
        d.line([(cols[0], top + k * 52), (cols[-1], top + k * 52)], fill=0, width=2)
    for x in cols:
        d.line([(x, top), (x, y)], fill=0, width=2)

    d.text((W - RIGHT - 420, y + 60), "综合管理部（档案室）", font=body, fill=0)
    d.text((W - RIGHT - 360, y + 115), "2025年10月9日", font=body, fill=0)
    img.save(out, optimize=True)


if __name__ == "__main__":
    main(sys.argv[1])
