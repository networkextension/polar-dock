# Dashboard UI 修改意见

## 一、必修 Bug
1. 「最近 Markdown」时间全部显示 `Invalid Date`，需在前端兜底：解析失败时显示 "—" 或相对时间，避免暴露异常字符串。
2. 左侧导航 `nav.teams` / `nav.projects` / `nav.iosDist` 是未翻译的 i18n key，必须补齐文案。
3. 中英混排不统一（Dashboard / Posts / Markdowns / Prompt 模板 / User Admin），需统一风格，建议参考「快速入口」九宫格的「英文标题 + 中文描述」。

## 二、布局与信息层级
4. 桌面 ≥1200px 应改为两列网格（如左：项目 / Markdown / Post，右：iOS 分发 / Chat），减少滚动距离。
5. 卡片标题（最近项目、最近 Markdown 等）字号字重和正文几乎相同，需要统一升级为 14–15px / 600 字重 / 高亮颜色，并与右上「查看全部 →」对齐成 header 行。
6. 「快速入口」九宫格卡片过高、信息密度低，建议压缩高度，并在右下补一个数量 metric（如「Markdown 38」），让入口同时承载概览功能。
7. 卡片之间 padding/margin 偏大，整体留白可适度收紧。

## 三、视觉与组件细节
8. 顶部 `Hello, adfdsfdsf`、`Administrator` 用了高饱和橙色胶囊，权重高于页面主标题。建议：用户名改普通文本+头像，角色标签改低饱和 outline，或合并到头像下拉菜单。
9. 主色橙色使用过度（顶部徽章 / 选中态 / 链接 / 按钮）。建议：品牌色仅保留在选中态和主 CTA，链接换中性蓝，徽章降饱和。
10. 列表行（最近 Markdown / iOS 分发 / 最近 Post）缺少 hover 反馈，需要加：背景变浅、cursor: pointer、右侧出现箭头。
11. 行内次要信息（`com.change.xxx`、日期）建议使用 monospace 字体并降低饱和，与主标题形成对比。
12. 左侧导航图标使用 Unicode 字符，粗细和基线不一致（✍ 明显偏大）。建议替换为统一图标库（Lucide / Tabler / Heroicons）。
13. 「New Markdown」白底黑字按钮与深色界面割裂，建议改为品牌色填充 + 白字 + `+` 图标；「⊙ 自定义」改为统一的 ghost outline 风格。
14. 次级文字（卡片标题、空状态文案、列表项标题）对比度偏低，需把灰度从 ~#6b6b6b 提到 ~#a0a0a0，主标题接近 #ECECEC，满足 WCAG AA 4.5:1。
15. 空状态太单薄（「还没有对话。」「还没有项目。」）。建议改为：图标插画 + 引导文案 + 主按钮 CTA。
16. 左侧底部「Markdown entries / New Tag / 搜索 / Chat / AI Assistant」与导航混在一起、层级不清，需用 divider + 小标题区分，或折叠到抽屉。

## 四、可访问性
17. 键盘 Tab 缺少 focus ring，需补 `:focus-visible` 2px outline 样式（适配深色）。
18. 确认语义层级：H1=Dashboard，每个卡片用 H2，列表项不用 heading 标签。
19. 橙色徽章上的白字、灰色正文等位置对比度全面校验一遍 WCAG AA。

## 五、响应式
20. 768px 断点下顶部 `Hello, adfdsfdsf` 徽章换行、`New Markdown` 主 CTA 被挤出视区。建议在 ≤900px 时把次要徽章收进头像菜单，保留主 CTA。
21. 移动端左侧栏收起后，汉堡按钮 `☰` 不够明显，建议加边框或背景以提升可发现性。

## 优先级 Top 5
1. 修复 Invalid Date
2. 提升次级文字对比度
3. 补齐 i18n key + 统一中英文风格
4. 桌面两列布局 + 列表行 hover 反馈
5. 收敛橙色使用 + 重做空状态
